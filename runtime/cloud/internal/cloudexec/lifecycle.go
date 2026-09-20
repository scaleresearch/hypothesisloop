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

// CreateWorkload rents the instance for exp's current attempt, in the slot admission placed the
// job on. It makes no placement decision of its own: desired state names the slot, and a slot
// that cannot take the job is an error, never a reason to pick another.
//
// An instance of that attempt that already exists, in any state, is the workload: one that has
// exited is waiting for the control plane to read its outcome, and renting another would run the
// job twice.
func (e *Executor) CreateWorkload(ctx context.Context, exp *domain.Experiment) error {
	spec, err := e.buildSpec(exp)
	if err != nil {
		return err
	}
	name := exp.EffectiveJob().NodeSelector[domain.NodeSlotLabel]
	if name == "" {
		return fmt.Errorf("cloudexec: experiment %s was admitted without a slot in its node selector (%s)", exp.ID, domain.NodeSlotLabel)
	}
	shape, index, ok := e.configured(name)
	if !ok {
		return fmt.Errorf("cloudexec: experiment %s was placed on slot %q, which is not configured", exp.ID, name)
	}
	if err := shape.holds(spec); err != nil {
		return fmt.Errorf("cloudexec: experiment %s on slot %s: %w", exp.ID, name, err)
	}
	v, err := e.current(ctx)
	if err != nil {
		return err
	}
	for _, i := range v.of(exp.ID) {
		if i.attempt == exp.AttemptCount {
			return nil
		}
	}
	for _, i := range v.instances {
		if i.slot() == name {
			return fmt.Errorf("cloudexec: experiment %s was placed on slot %s, which instance %s still holds", exp.ID, name, i.Name)
		}
	}
	spec.Name = e.instanceName(exp.ID, exp.AttemptCount, shape.Name, index)
	if err := provider.ValidateName(spec.Name); err != nil {
		return fmt.Errorf("cloudexec: experiment %s: %w", exp.ID, err)
	}
	id, err := e.provider.Create(ctx, spec)
	if err != nil {
		return fmt.Errorf("cloudexec: create instance %s: %w", spec.Name, err)
	}
	v.add(instance{
		Instance:     provider.Instance{ID: id, Name: spec.Name, State: provider.StateStarting},
		experimentID: exp.ID, attempt: exp.AttemptCount, shape: shape.Name, index: index,
	})
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
	v, err := e.current(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, i := range v.of(experimentID) {
		if grantCheckpointWindow && i.State == provider.StateRunning {
			if err := e.provider.Stop(ctx, i.ID); err != nil {
				errs = append(errs, fmt.Errorf("cloudexec: stop instance %s: %w", i.Name, err))
			}
			continue
		}
		if err := e.destroy(ctx, v, i); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (e *Executor) destroy(ctx context.Context, v *view, i instance) error {
	if err := e.provider.Destroy(ctx, i.ID); err != nil {
		return fmt.Errorf("cloudexec: destroy instance %s: %w", i.Name, err)
	}
	v.remove(i.ID)
	return nil
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
	v, err := e.current(ctx)
	if err != nil {
		return nil, err
	}
	var ids []string
	seen := map[string]bool{}
	for _, i := range v.instances {
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
	v, err := e.current(ctx)
	if err != nil {
		return false, err
	}
	matches := false
	for _, i := range v.of(exp.ID) {
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
//
// An exited instance is terminal only once its output has been read and holds the wrapper's exit
// line. Two cases are not, and neither is allowed to fail the poll, since one job that cannot be
// polled withholds the whole cluster's status push:
//
//   - The output cannot be read. How the command ended is unknown, so the job is reported still
//     running and the read is tried again on the next push. A job that stays unreadable sends no
//     metrics either, which the control plane already evicts as silent.
//   - The output is readable and has no exit line. The wrapper died with its machine, so the
//     workload is reported gone, the same as an instance the vendor removed: the control plane
//     requeues that as an infrastructure fault and the job's own retries are not spent.
func (e *Executor) PollJobStatus(ctx context.Context, experimentID string) (agentexec.JobStatus, error) {
	v, err := e.current(ctx)
	if err != nil {
		return agentexec.JobStatus{Phase: workload.JobPhasePending, Attempt: workload.AttemptUnknown}, err
	}
	i, ok := v.newest(experimentID)
	if !ok {
		return agentexec.JobStatus{Phase: workload.JobPhaseGone, Attempt: workload.AttemptUnknown}, nil
	}
	observed := agentexec.JobStatus{Phase: workload.JobPhasePending, UID: i.Name, Attempt: i.attempt}
	switch i.State {
	case provider.StateRunning:
		observed.Phase = workload.JobPhaseRunning
	case provider.StateExited:
		how, _, _ := e.ending(ctx, v, i)
		observed.Phase = map[ending]workload.JobPhase{
			endingUnread:     workload.JobPhaseRunning,
			endingSucceeded:  workload.JobPhaseSucceeded,
			endingFailed:     workload.JobPhaseFailed,
			endingUnreported: workload.JobPhaseGone,
		}[how]
	}
	return observed, nil
}

// ResolveAdmittedAcceleratorType reports the type and node of the slot experimentID's instance
// occupies.
func (e *Executor) ResolveAdmittedAcceleratorType(ctx context.Context, experimentID string) (domain.AcceleratorType, string, bool, error) {
	v, err := e.current(ctx)
	if err != nil {
		return "", "", true, err
	}
	i, ok := v.newest(experimentID)
	if !ok {
		return "", "", true, nil
	}
	shape, _, _ := e.configured(i.slot())
	return domain.AcceleratorType(shape.AcceleratorType), i.slot(), true, nil
}

// ReapTerminal destroys what no pass has a use for any more: every instance of an attempt beyond
// the first, and every exited instance that is not the current attempt of a desired experiment,
// which is a job the control plane has finished with or the attempt a requeue left behind. One
// failed destroy does not keep the rest rented.
//
// A second instance of one attempt is a create the vendor carried out and reported failed, or a
// list that lagged behind a create. The lowest id is kept, which is also the one every status
// read reports, so which of the two survives does not depend on the pass that finds them.
func (e *Executor) ReapTerminal(ctx context.Context, desired map[string]*domain.Experiment) error {
	v, err := e.current(ctx)
	if err != nil {
		return err
	}
	type attempt struct {
		experimentID string
		number       int
	}
	kept := map[attempt]bool{}
	var errs []error
	for _, i := range append([]instance(nil), v.instances...) {
		key := attempt{i.experimentID, i.attempt}
		exp := desired[i.experimentID]
		current := exp != nil && exp.AttemptCount == i.attempt
		if !kept[key] && (i.live() || current) {
			kept[key] = true
			continue
		}
		if err := e.destroy(ctx, v, i); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
