package scheduler

import (
	"fmt"
	"log"

	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
)

const (
	// maxScheduleAttempts is used to limit the number of times
	// we will attempt to schedule if we continue to hit conflicts.
	maxScheduleAttempts = 5
)

// ServiceScheduler is used for 'service' type jobs. This scheduler is
// designed for long-lived services, and as such spends more time attemping
// to make a high quality placement. This is the primary scheduler for
// most workloads.
type ServiceScheduler struct {
	logger  *log.Logger
	state   State
	planner Planner

	eval *structs.Evaluation
	plan *structs.Plan
}

// NewServiceScheduler is a factory function to instantiate a new service scheduler
func NewServiceScheduler(logger *log.Logger, state State, planner Planner) Scheduler {
	s := &ServiceScheduler{
		logger:  logger,
		state:   state,
		planner: planner,
	}
	return s
}

// Process is used to handle a single evaluation
func (s *ServiceScheduler) Process(eval *structs.Evaluation) error {
	// Verify the evaluation trigger reason is understood
	switch eval.TriggeredBy {
	case structs.EvalTriggerJobRegister, structs.EvalTriggerNodeUpdate,
		structs.EvalTriggerJobDeregister:
	default:
		return fmt.Errorf("service scheduler cannot handle '%s' evaluation reason",
			eval.TriggeredBy)
	}

	// Store the evaluation
	s.eval = eval

	// Retry up to the maxScheduleAttempts
	return retryMax(maxScheduleAttempts, s.process)
}

// process is wrapped in retryMax to iteratively run the handler until we have no
// further work or we've made the maximum number of attempts.
func (s *ServiceScheduler) process() (bool, error) {
	// Lookup the Job by ID
	job, err := s.state.GetJobByID(s.eval.JobID)
	if err != nil {
		return false, fmt.Errorf("failed to get job '%s': %v",
			s.eval.JobID, err)
	}

	// Create a plan
	s.plan = s.eval.MakePlan(job)

	// Compute the target job allocations
	if err := s.computeJobAllocs(job); err != nil {
		s.logger.Printf("[ERR] sched: %#v: %v", s.eval, err)
		return false, err
	}

	// Submit the plan
	result, newState, err := s.planner.SubmitPlan(s.plan)
	if err != nil {
		return false, err
	}

	// If we got a state refresh, try again since we have stale data
	if newState != nil {
		s.logger.Printf("[DEBUG] sched: %#v: refresh forced", s.eval)
		s.state = newState
		return false, nil
	}

	// Try again if the plan was not fully committed, potential conflict
	fullCommit, expected, actual := result.FullCommit(s.plan)
	if !fullCommit {
		s.logger.Printf("[DEBUG] sched: %#v: attempted %d placements, %d placed",
			s.eval, expected, actual)
		return false, nil
	}

	// Success!
	return true, nil
}

// computeJobAllocs is used to reconcile differences between the job,
// existing allocations and node status to update the allocations.
func (s *ServiceScheduler) computeJobAllocs(job *structs.Job) error {
	// Materialize all the task groups, job could be missing if deregistered
	var groups map[string]*structs.TaskGroup
	if job != nil {
		groups = materializeTaskGroups(job)
	}

	// Lookup the allocations by JobID
	allocs, err := s.state.AllocsByJob(s.eval.JobID)
	if err != nil {
		return fmt.Errorf("failed to get allocs for job '%s': %v",
			s.eval.JobID, err)
	}

	// Determine the tainted nodes containing job allocs
	tainted, err := taintedNodes(s.state, allocs)
	if err != nil {
		return fmt.Errorf("failed to get tainted nodes for job '%s': %v",
			s.eval.JobID, err)
	}

	// Index the existing allocations
	indexed := indexAllocs(allocs)

	// Diff the required and existing allocations
	place, update, migrate, evict, ignore := diffAllocs(job, tainted, groups, indexed)
	s.logger.Printf("[DEBUG] sched: %#v: need %d placements, %d updates, %d migrations, %d evictions, %d ignored allocs",
		s.eval, len(place), len(update), len(migrate), len(evict), len(ignore))

	// Fast-pass if nothing to do
	if len(place) == 0 && len(update) == 0 && len(evict) == 0 && len(migrate) == 0 {
		return nil
	}

	// Add all the evicts
	addEvictsToPlan(s.plan, evict, indexed)

	// For simplicity, we treat all migrates as an evict + place.
	// XXX: This could probably be done more intelligently?
	addEvictsToPlan(s.plan, migrate, indexed)
	place = append(place, migrate...)

	// For simplicity, we treat all updates as an evict + place.
	// XXX: This should be done with rolling in-place updates instead.
	addEvictsToPlan(s.plan, update, indexed)
	place = append(place, update...)

	// Nothing remaining to do if placement is not required
	if len(place) == 0 {
		return nil
	}

	// Create an evaluation context
	ctx := NewEvalContext(s.state, s.plan, s.logger)

	// Get the base nodes
	nodes, err := readyNodesInDCs(s.state, job.Datacenters)
	if err != nil {
		return err
	}

	// Construct the placement stack
	stack := NewServiceStack(ctx, nodes)

	for _, missing := range place {
		stack.SetTaskGroup(groups[missing.Name])
		option := stack.Select()
		if option == nil {
			s.logger.Printf("[DEBUG] sched: %#v: failed to place alloc %s",
				s.eval, missing)
			continue
		}

		// Create an allocation for this
		alloc := &structs.Allocation{
			ID:        mock.GenerateUUID(),
			Name:      missing.Name,
			NodeID:    option.Node.ID,
			JobID:     job.ID,
			Job:       job,
			Resources: nil, // TODO: size
			Metrics:   nil,
			Status:    structs.AllocStatusPending,
		}
		s.plan.AppendAlloc(alloc)
	}
	return nil
}
