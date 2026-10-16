package cloudexec

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/contract/exitline"
	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/agentexec"
)

// workloadOf reports whether i is exp's workload: the instance of its current attempt in the slot
// admission pinned it to. The attempt alone does not say so. A preempted job is readmitted under
// the attempt it had, and the instance it left in another slot would otherwise pass for it, and
// that instance's exit for the job's result.
func workloadOf(exp *clusterapi.DesiredJob, i instance) bool {
	return i.experimentID == exp.ID && i.attempt == exp.AttemptCount && i.slot() == exp.EffectiveJob().NodeSelector[clusterapi.NodeSlotLabel]
}

// CreateWorkload rents the instance for exp's current attempt, in the slot admission placed the
// job on. It makes no placement decision of its own: desired state names the slot, and a slot
// that cannot take the job is an error, never a reason to pick another.
//
// The instance of that attempt in that slot, in any state, is the workload: one that has exited
// is waiting for the control plane to read its outcome, and renting another would run the job
// twice. Any other instance of the experiment is left over from an earlier attempt or an earlier
// placement. An exited one is destroyed before the new instance is rented, so a status read
// never finds two instances of one job to choose between. A live one is the reconcile pass's to
// delete first, and until it has, nothing is rented.
//
// An instance that boots an environment pulls it with pull, the reconcile answer's registry_pull.
func (e *Executor) CreateWorkload(ctx context.Context, exp *clusterapi.DesiredJob, pull *clusterapi.RegistryLogin) error {
	spec, err := e.buildSpec(exp)
	if err != nil {
		return err
	}
	if spec.Image != e.jobImage {
		if pull == nil {
			return fmt.Errorf("cloudexec: experiment %s runs in an environment and the reconcile answer has no registry_pull", exp.ID)
		}
		spec.ImageLogin = &provider.ImageLogin{Registry: pull.Registry, Username: pull.Username, Password: pull.Password}
	}
	name := exp.EffectiveJob().NodeSelector[clusterapi.NodeSlotLabel]
	if name == "" {
		return fmt.Errorf("cloudexec: experiment %s was admitted without a slot in its node selector (%s)", exp.ID, clusterapi.NodeSlotLabel)
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
		switch {
		case workloadOf(exp, i):
			return nil
		case i.live():
			return fmt.Errorf("cloudexec: experiment %s is placed on slot %s while instance %s of it still runs", exp.ID, name, i.Name)
		}
		if err := e.destroy(ctx, v, i, "it exited and is left from an earlier attempt or placement of its job"); err != nil {
			return err
		}
	}
	for _, i := range v.instances {
		if i.slot() == name {
			return fmt.Errorf("cloudexec: experiment %s was placed on slot %s, which instance %s still holds", exp.ID, name, i.Name)
		}
	}
	spec.Name = e.instanceName(exp.ID, exp.AttemptCount, shape.Name, index)
	spec.AvoidHosts = v.unstarting
	if err := provider.ValidateName(spec.Name); err != nil {
		return fmt.Errorf("cloudexec: experiment %s: %w", exp.ID, err)
	}
	id, err := e.provider.Create(ctx, spec)
	if errors.Is(err, provider.ErrNoOffer) {
		return e.withoutOffer(exp, shape)
	}
	if err != nil {
		return fmt.Errorf("cloudexec: create instance %s: %w", spec.Name, err)
	}
	log.Printf("cloudexec: created instance %s (%s): attempt %d of experiment %s is desired in slot %s", spec.Name, id, exp.AttemptCount, exp.ID, name)
	v.add(instance{
		Instance:     provider.Instance{ID: id, Name: spec.Name, State: provider.StateStarting},
		experimentID: exp.ID, attempt: exp.AttemptCount, shape: shape.Name, index: index,
	})
	return nil
}

// withoutOffer is what a job is told when there is no machine to rent for it: that it waits,
// and once it has waited OfferWait since its admission, that this cluster has none for it. The
// control plane then queues the job again, where it waits for a slot that has an offer.
func (e *Executor) withoutOffer(exp *clusterapi.DesiredJob, shape Shape) error {
	if exp.SubmittedAt == nil {
		return fmt.Errorf("cloudexec: experiment %s is placed here with no admission time", exp.ID)
	}
	what := fmt.Sprintf("no machine to rent for shape %s (%s) within the provider's configured filters", shape.Name, cmp.Or(shape.AcceleratorType, "no accelerator"))
	if time.Since(*exp.SubmittedAt) >= e.offerWait {
		return &agentexec.Unrunnable{Reason: clusterapi.PhaseReasonNoOffer, Why: fmt.Sprintf("%s in %s of asking", what, e.offerWait)}
	}
	return &agentexec.Waiting{Reason: clusterapi.PhaseReasonWaitingForOffer, Why: fmt.Sprintf("%s yet; the job goes back to the queue as no_offer if none comes within %s of its admission", what, e.offerWait)}
}

// overdue says i was rented longer ago than Startup and has not started its command. An instance
// the provider lists as unknown is never overdue: its command may be hours in, and StartedAt is
// when it was rented.
func (e *Executor) overdue(i instance) bool {
	return i.State == provider.StateStarting && !i.StartedAt.IsZero() && time.Since(i.StartedAt) > e.startup
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
// started, one the provider does not know to be running, which a signal may not reach, and an
// instance still running after the control plane stopped listing its grant, which bounds an
// instance whose wrapper never ends it.
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
		if err := e.destroy(ctx, v, i, "the workload of its experiment was deleted while it was "+string(i.State)); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (e *Executor) destroy(ctx context.Context, v *view, i instance, why string) error {
	if err := e.provider.Destroy(ctx, i.ID); err != nil {
		return fmt.Errorf("cloudexec: destroy instance %s (%s): %s: %w", i.Name, i.ID, why, err)
	}
	log.Printf("cloudexec: destroyed instance %s (%s): %s", i.Name, i.ID, why)
	v.remove(i.ID)
	return nil
}

// Leave is what the process does last, after its loop has ended: it says by name what is still
// rented under this namespace, which the next start of this runtime finds in the vendor's list
// and reconciles, a destroy that failed among it.
func (e *Executor) Leave(ctx context.Context) error {
	listed, err := e.provider.List(ctx, e.prefix)
	if err != nil {
		return fmt.Errorf("cloudexec: what is still rented could not be listed: %w", err)
	}
	for _, i := range listed {
		log.Printf("cloudexec: leaving instance %s (%s) rented, %s", i.Name, i.ID, i.State)
	}
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

// WorkloadMatchesDesired is true when every live instance of exp is its workload: of its current
// attempt and in the slot it is pinned to. Both are in the name, which is all an instance can be
// compared on. A requeue changes the first, a readmission the second.
//
// An instance that has not started within Startup does not match either: the pass destroys it
// and the next rents another, until the control plane's own deadline for a job that is not
// running requeues or ends the job.
//
// An instance the provider does not know to be running matches: it is reported as pending, and
// the control plane, which hears nothing of a job that ran and gives one that never ran its time
// to start, requeues or ends the job. Only then is the instance destroyed.
//
// The workload of a desired job in a slot the configuration no longer has, as a renamed shape or
// a lowered max_instances leaves one, is an error and is left running: it takes no slot, so it is
// billed beside a full account, and destroying it would end a job the control plane still wants.
// The same instance of a job that is no longer desired is deleted like any other.
func (e *Executor) WorkloadMatchesDesired(ctx context.Context, exp *clusterapi.DesiredJob) (bool, error) {
	v, err := e.current(ctx)
	if err != nil {
		return false, err
	}
	matches := false
	for _, i := range v.of(exp.ID) {
		if !i.live() {
			continue
		}
		if !workloadOf(exp, i) {
			return false, nil
		}
		if _, _, ok := e.configured(i.slot()); !ok {
			return false, fmt.Errorf("cloudexec: instance %s runs experiment %s in slot %s, which no configured shape has, so it is billed beside every slot: restore the shape or its max_instances, or end the job", i.Name, exp.ID, i.slot())
		}
		if e.overdue(i) {
			log.Printf("cloudexec: instance %s has not started in %s and is replaced", i.Name, e.startup)
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
// line. Without the line the workload is reported gone, as an instance the vendor removed is,
// whether the output has no line (the wrapper died with its machine) or cannot be read at all.
// Neither fails the poll, since one job that cannot be polled withholds the whole cluster's
// status push. The control plane confirms a gone report over several snapshots before it acts,
// which is what lets a read that fails once recover, and then requeues the job as an
// infrastructure fault: its own retries are not spent, and nothing is finished on a guess.
//
// Reporting an unreadable instance as still running instead is not bounded by anything: every
// running report is recorded as liveness, so the job never reads as silent. For the same reason
// an instance the provider does not know to be running is reported as pending, like one that is
// starting: the control plane bounds a job that ran and reads pending with no sign of life.
//
// The vendor's state says what the machine does, at the pace of the vendor's own reports and of
// this runtime's reads of them, and a command can start and end between two of those. When the
// command ran is therefore the wrapper's word, read from the output with the exit line.
func (e *Executor) PollJobStatus(ctx context.Context, experimentID string) (agentexec.JobStatus, error) {
	v, err := e.current(ctx)
	if err != nil {
		return agentexec.JobStatus{Phase: clusterapi.JobPhasePending, Attempt: clusterapi.AttemptUnknown}, err
	}
	i, ok := v.newest(experimentID)
	if !ok {
		return agentexec.JobStatus{Phase: clusterapi.JobPhaseGone, Attempt: clusterapi.AttemptUnknown}, nil
	}
	observed := agentexec.JobStatus{Phase: clusterapi.JobPhasePending, UID: i.Name, Attempt: i.attempt, Machine: &clusterapi.JobMachine{
		InstanceID: i.ID, HostID: i.Host, AcceleratorModel: i.AcceleratorModel, CPUModel: i.CPUModel, PricePerHourUSD: i.PricePerHour,
	}}
	switch i.State {
	case provider.StateRunning:
		observed.Phase = clusterapi.JobPhaseRunning
	case provider.StateExited:
		how, _, readErr := e.ending(ctx, v, i)
		if readErr != nil {
			log.Printf("cloudexec: reporting %s as gone: %v", i.Name, readErr)
		}
		if how != endingUnknown {
			observed.StartedAt, observed.EndedAt, _ = exitline.FindRun(e.output(ctx, v, i).lines)
		}
		observed.Phase = map[ending]clusterapi.JobPhase{
			endingUnknown:   clusterapi.JobPhaseGone,
			endingSucceeded: clusterapi.JobPhaseSucceeded,
			endingFailed:    clusterapi.JobPhaseFailed,
		}[how]
	}
	return observed, nil
}

// ResolveAdmittedAcceleratorType reports the type and node of the slot experimentID's instance
// occupies.
func (e *Executor) ResolveAdmittedAcceleratorType(ctx context.Context, experimentID string) (clusterapi.AcceleratorType, string, bool, error) {
	v, err := e.current(ctx)
	if err != nil {
		return "", "", true, err
	}
	i, ok := v.newest(experimentID)
	if !ok {
		return "", "", true, nil
	}
	shape, _, _ := e.configured(i.slot())
	return clusterapi.AcceleratorType(shape.AcceleratorType), i.slot(), true, nil
}

// ReapTerminal destroys what no pass has a use for any more: every instance of one name beyond
// the first, every exited instance that is not the workload of a desired experiment, which is a
// job the control plane has finished with, or what a requeue or a readmission to another slot
// left behind, and every instance under this namespace's prefix whose name is no job's. One
// failed destroy does not keep the rest rented.
//
// A second instance of one name is a create the vendor carried out and reported failed, or a
// list that lagged behind a create. The lowest id is kept, which is also the one every status
// read reports, so which of the two survives does not depend on the pass that finds them.
func (e *Executor) ReapTerminal(ctx context.Context, desired map[string]*clusterapi.DesiredJob) error {
	v, err := e.current(ctx)
	if err != nil {
		return err
	}
	kept := map[string]bool{}
	var errs []error
	for _, i := range append([]instance(nil), v.instances...) {
		exp := desired[i.experimentID]
		wanted := i.live() || (exp != nil && workloadOf(exp, i))
		if wanted && !kept[i.Name] {
			kept[i.Name] = true
			continue
		}
		why := "it exited and is not the workload of a desired experiment"
		if wanted {
			why = "it is a second instance of its name"
		}
		if err := e.destroy(ctx, v, i, why); err != nil {
			errs = append(errs, err)
		}
	}
	for _, i := range append([]instance(nil), v.unnamed...) {
		if err := e.destroy(ctx, v, i, "it is under this namespace's prefix and its name is no job's"); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
