package cloudexec

import (
	"context"
	"errors"
	"fmt"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/workload"
	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/agentexec"
)

// CreateWorkload rents the instance for exp's current attempt. An instance of that attempt that
// already exists, in any state, is the workload: one that has exited is waiting for the control
// plane to read its outcome, and renting another would run the job twice.
func (e *Executor) CreateWorkload(ctx context.Context, exp *domain.Experiment) error {
	spec, err := e.buildSpec(exp)
	if err != nil {
		return err
	}
	instances, err := e.list(ctx)
	if err != nil {
		return err
	}
	for _, i := range of(instances, exp.ID) {
		if i.attempt == exp.AttemptCount {
			return nil
		}
	}
	shape, slot, err := e.freeSlot(instances, spec)
	if err != nil {
		return fmt.Errorf("%w (experiment %s)", err, exp.ID)
	}
	spec.Name = instanceName(exp.ID, exp.AttemptCount, shape, slot)
	if _, err := e.provider.Create(ctx, spec); err != nil {
		return fmt.Errorf("cloudexec: create instance %s: %w", spec.Name, err)
	}
	return nil
}

// DeleteWorkload ends experimentID's instances. The control plane's grant is acted on as it is
// read, with nothing kept from one pass to the next:
//
// A running instance whose termination was granted the checkpoint window is sent SIGTERM and
// left alone. The wrapper inside passes the signal on and ends the command when the window it was
// started with runs out, the instance exits, and ReapTerminal destroys it. Until then every pass
// arrives here again and stops it again, which the wrapper ignores after the first.
//
// Everything else is destroyed at once: a termination with no grant, an instance that never
// started, and an instance still running after the control plane stopped listing its grant,
// which bounds an instance whose wrapper never ends it.
func (e *Executor) DeleteWorkload(ctx context.Context, experimentID string, grantCheckpointWindow bool) error {
	instances, err := e.list(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, i := range of(instances, experimentID) {
		if grantCheckpointWindow && i.State == provider.StateRunning {
			if err := e.provider.Stop(ctx, i.ID); err != nil {
				errs = append(errs, fmt.Errorf("cloudexec: stop instance %s: %w", i.Name, err))
			}
			continue
		}
		if err := e.provider.Destroy(ctx, i.ID); err != nil {
			errs = append(errs, fmt.Errorf("cloudexec: destroy instance %s: %w", i.Name, err))
		}
	}
	return errors.Join(errs...)
}

// ListManagedJobs returns the experiments with an instance whose command has not ended. An
// exited instance is not actual: the pass must neither compare it with desired state nor stop it.
func (e *Executor) ListManagedJobs(ctx context.Context) ([]string, error) {
	return e.experimentIDs(ctx, instance.live)
}

// ListManagedJobsForStatus returns every experiment with an instance, so an exited one is
// reported with its outcome until ReapTerminal destroys it.
func (e *Executor) ListManagedJobsForStatus(ctx context.Context) ([]string, error) {
	return e.experimentIDs(ctx, func(instance) bool { return true })
}

func (e *Executor) experimentIDs(ctx context.Context, include func(instance) bool) ([]string, error) {
	instances, err := e.list(ctx)
	if err != nil {
		return nil, err
	}
	var ids []string
	seen := map[string]bool{}
	for _, i := range instances {
		if include(i) && !seen[i.experimentID] {
			seen[i.experimentID] = true
			ids = append(ids, i.experimentID)
		}
	}
	return ids, nil
}

// ListManagedAuxiliaryWorkloads has nothing to list: an instance is the whole workload.
func (e *Executor) ListManagedAuxiliaryWorkloads(context.Context) ([]string, error) {
	return nil, nil
}

// WorkloadMatchesDesired is true when every live instance of exp is of its current attempt. The
// attempt in the name is all an instance can be compared on, and it is what a requeue changes.
func (e *Executor) WorkloadMatchesDesired(ctx context.Context, exp *domain.Experiment) (bool, error) {
	instances, err := e.list(ctx)
	if err != nil {
		return false, err
	}
	matches := false
	for _, i := range of(instances, exp.ID) {
		if !i.live() {
			continue
		}
		if i.attempt != exp.AttemptCount {
			return false, nil
		}
		matches = true
	}
	return matches, nil
}

// PollJobStatus reports experimentID's newest instance. The name stands in for a UID: it differs
// between attempts and between slots.
func (e *Executor) PollJobStatus(ctx context.Context, experimentID string) (agentexec.JobStatus, error) {
	instances, err := e.list(ctx)
	if err != nil {
		return agentexec.JobStatus{Phase: workload.JobPhasePending, Attempt: workload.AttemptUnknown}, err
	}
	i, ok := newest(instances, experimentID)
	if !ok {
		return agentexec.JobStatus{Phase: workload.JobPhaseGone, Attempt: workload.AttemptUnknown}, nil
	}
	observed := agentexec.JobStatus{Phase: workload.JobPhasePending, UID: i.Name, Attempt: i.attempt}
	switch i.State {
	case provider.StateRunning:
		observed.Phase = workload.JobPhaseRunning
	case provider.StateExited:
		outcome, err := e.outcome(ctx, i)
		if err != nil {
			return observed, err
		}
		observed.Phase = workload.JobPhaseFailed
		if outcome.reported && outcome.code == 0 {
			observed.Phase = workload.JobPhaseSucceeded
		}
	}
	return observed, nil
}

// ResolveAdmittedAcceleratorType reports the type and node of the slot experimentID's live
// instance occupies.
func (e *Executor) ResolveAdmittedAcceleratorType(ctx context.Context, experimentID string) (domain.AcceleratorType, string, bool, error) {
	instances, err := e.list(ctx)
	if err != nil {
		return "", "", true, err
	}
	i, ok := newest(instances, experimentID)
	if !ok || !i.live() {
		return "", "", true, nil
	}
	for _, s := range e.shapes {
		if s.Name == i.shape {
			return domain.AcceleratorType(s.AcceleratorType), i.node(), true, nil
		}
	}
	return "", i.node(), true, nil
}

// ReapTerminal destroys every exited instance that is not the current attempt of a desired
// experiment: a job the control plane has finished with, and the attempt a requeue left behind.
// One failed destroy does not keep the rest rented (important.md #19).
func (e *Executor) ReapTerminal(ctx context.Context, desired map[string]*domain.Experiment) error {
	instances, err := e.list(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, i := range instances {
		if i.live() {
			continue
		}
		if exp := desired[i.experimentID]; exp != nil && exp.AttemptCount == i.attempt {
			continue
		}
		if err := e.provider.Destroy(ctx, i.ID); err != nil {
			errs = append(errs, fmt.Errorf("cloudexec: destroy exited instance %s: %w", i.Name, err))
		}
	}
	return errors.Join(errs...)
}
