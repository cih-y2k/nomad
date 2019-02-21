package taskrunner

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"runtime"

	hclog "github.com/hashicorp/go-hclog"
	plugin "github.com/hashicorp/go-plugin"
	"github.com/hashicorp/nomad/client/allocrunner/interfaces"
	"github.com/hashicorp/nomad/client/logmon"
	"github.com/hashicorp/nomad/helper/uuid"
	pstructs "github.com/hashicorp/nomad/plugins/shared/structs"
)

const (
	// logmonReattachKey is the HookData key where logmon's reattach config
	// is stored.
	logmonReattachKey = "reattach_config"
)

// logmonHook launches logmon and manages task logging
type logmonHook struct {
	// logmon is the handle to the log monitor process for the task.
	logmon             logmon.LogMon
	logmonPluginClient *plugin.Client

	config *logmonHookConfig

	logger hclog.Logger
}

type logmonHookConfig struct {
	logDir     string
	stdoutFifo string
	stderrFifo string
}

func newLogMonHook(cfg *logmonHookConfig, logger hclog.Logger) *logmonHook {
	hook := &logmonHook{
		config: cfg,
		logger: logger,
	}

	return hook
}

func newLogMonHookConfig(taskName, logDir string) *logmonHookConfig {
	cfg := &logmonHookConfig{
		logDir: logDir,
	}
	if runtime.GOOS == "windows" {
		id := uuid.Generate()[:8]
		cfg.stdoutFifo = fmt.Sprintf("//./pipe/%s-%s.stdout", taskName, id)
		cfg.stderrFifo = fmt.Sprintf("//./pipe/%s-%s.stderr", taskName, id)
	} else {
		cfg.stdoutFifo = filepath.Join(logDir, fmt.Sprintf(".%s.stdout.fifo", taskName))
		cfg.stderrFifo = filepath.Join(logDir, fmt.Sprintf(".%s.stderr.fifo", taskName))
	}
	return cfg
}

func (*logmonHook) Name() string {
	return "logmon"
}

func (h *logmonHook) launchLogMon(reattachConfig *plugin.ReattachConfig) error {
	l, c, err := logmon.LaunchLogMon(h.logger, reattachConfig)
	if err != nil {
		return err
	}

	h.logmon = l
	h.logmonPluginClient = c
	return nil
}

func reattachConfigFromHookData(data map[string]string) (*plugin.ReattachConfig, error) {
	if data == nil || data[logmonReattachKey] == "" {
		return nil, nil
	}

	var cfg pstructs.ReattachConfig
	err := json.Unmarshal([]byte(data[logmonReattachKey]), &cfg)
	if err != nil {
		return nil, err
	}

	return pstructs.ReattachConfigToGoPlugin(&cfg)
}

func (h *logmonHook) Prestart(ctx context.Context,
	req *interfaces.TaskPrestartRequest, resp *interfaces.TaskPrestartResponse) error {

	reattachConfig, err := reattachConfigFromHookData(req.PreviousState)
	if err != nil {
		h.logger.Error("failed to load reattach config", "error", err)
		return err
	}

	// Launch or reattach logmon instance for the task.
	if err := h.launchLogMon(reattachConfig); err != nil {
		h.logger.Error("failed to launch logmon process", "error", err)
		return err
	}

	// Only tell logmon to start when we are not reattaching to a running instance
	if reattachConfig == nil {
		err := h.logmon.Start(&logmon.LogConfig{
			LogDir:        h.config.logDir,
			StdoutLogFile: fmt.Sprintf("%s.stdout", req.Task.Name),
			StderrLogFile: fmt.Sprintf("%s.stderr", req.Task.Name),
			StdoutFifo:    h.config.stdoutFifo,
			StderrFifo:    h.config.stderrFifo,
			MaxFiles:      req.Task.LogConfig.MaxFiles,
			MaxFileSizeMB: req.Task.LogConfig.MaxFileSizeMB,
		})
		if err != nil {
			h.logger.Error("failed to start logmon", "error", err)
			return err
		}
	}

	rCfg := pstructs.ReattachConfigFromGoPlugin(h.logmonPluginClient.ReattachConfig())
	jsonCfg, err := json.Marshal(rCfg)
	if err != nil {
		return err
	}
	resp.State = map[string]string{logmonReattachKey: string(jsonCfg)}
	return nil
}

func (h *logmonHook) Stop(_ context.Context, req *interfaces.TaskStopRequest, _ *interfaces.TaskStopResponse) error {

	// It's possible that Stop was called without calling Prestart on agent
	// restarts. Attempt to reattach to an existing logmon.
	if h.logmon == nil || h.logmonPluginClient == nil {
		h.reattach(req)
	}

	if h.logmon != nil {
		h.logmon.Stop()
	}
	if h.logmonPluginClient != nil {
		h.logmonPluginClient.Kill()
	}

	return nil
}

// reattach to a running logmon if possible. Will not start a new logmon.
func (h *logmonHook) reattach(req *interfaces.TaskStopRequest) error {
	reattachConfig, err := reattachConfigFromHookData(req.ExistingState)
	if err != nil {
		h.logger.Error("failed to load reattach config", "error", err)
		return err
	}

	// Give up if there's no reattach config
	if reattachConfig == nil {
		return nil
	}

	return h.launchLogMon(reattachConfig)
}
