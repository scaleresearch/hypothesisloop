package compute

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/agentexec"
)

// workloadOf reports whether i is exp's workload: of its current attempt, one of its ranks, in the
// slot admission pinned that rank to. A readmission keeps the attempt, so the slot is what tells
// the instance a preempted job left elsewhere from its workload.
func workloadOf(exp *clusterapi.DesiredJob, i instance) bool {
	slots := exp.EffectiveJob().Slots
	return i.experimentID == exp.ID && i.attempt == exp.AttemptCount && i.ranks == len(slots) && i.slot() == slots[i.rank]
}

// masterSelf is the address rank 0 of a gang is told it meets the others at. Its private address
// is known only once it is rented, after its environment is fixed.
const masterSelf = "127.0.0.1"

// CreateWorkload rents the instances of exp's current attempt, one per rank, each in the slot
// admission pinned it to. The attempt's instances in those slots, exited ones included, are the
// workload and are never rented twice. An exited instance of another attempt or placement is
// destroyed first, so a status read finds one placement; a live one blocks renting until the pass
// deletes it. A gang is rented whole or not at all (undo). A gang whose place has no machine left
// for a rank once rank 0 is rented goes back to the queue at once, so no pass rents and destroys
// rank 0 again within one admission.
func (e *Executor) CreateWorkload(ctx context.Context, exp *clusterapi.DesiredJob) error {
	ranks, err := e.buildRanks(exp)
	if err != nil {
		return err
	}
	if len(ranks) > 1 && e.network == nil {
		return &agentexec.Unrunnable{Why: fmt.Sprintf("the job asks for %d nodes, and this cluster's provider has no private network to join them on", len(ranks))}
	}
	job := exp.EffectiveJob()
	slots := job.Slots
	switch {
	case len(slots) == 0:
		return fmt.Errorf("cloudcompute: experiment %s was admitted without a slot", exp.ID)
	case len(slots) != len(ranks):
		return fmt.Errorf("cloudcompute: experiment %s of %d nodes was admitted to %d slots", exp.ID, len(ranks), len(slots))
	}
	shapes := make([]Shape, len(ranks))
	for r, name := range slots {
		shape, index, ok := e.configured(name)
		if !ok {
			return fmt.Errorf("cloudcompute: experiment %s was placed on slot %q, which is not configured", exp.ID, name)
		}
		if err := shape.holds(ranks[r].spec); err != nil {
			return fmt.Errorf("cloudcompute: experiment %s on slot %s: %w", exp.ID, name, err)
		}
		shapes[r] = shape
		ranks[r].spec.Name = e.instanceName(exp.ID, exp.AttemptCount, r, len(ranks), shape.Name, index)
		if err := provider.ValidateName(ranks[r].spec.Name); err != nil {
			return fmt.Errorf("cloudcompute: experiment %s: %w", exp.ID, err)
		}
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
			return fmt.Errorf("cloudcompute: experiment %s is placed on slots %s while instance %s of it still runs", exp.ID, strings.Join(slots, ", "), i.Name)
		}
		if err := e.destroy(ctx, v, i, "it exited and is left from an earlier attempt or placement of its job"); err != nil {
			return err
		}
	}
	for _, i := range v.instances {
		if slices.Contains(slots, i.slot()) {
			return fmt.Errorf("cloudcompute: experiment %s was placed on slot %s, which instance %s still holds", exp.ID, i.slot(), i.Name)
		}
	}
	place := ""
	if len(ranks) > 1 {
		specs := make([]provider.Spec, len(ranks))
		for r := range ranks {
			specs[r] = ranks[r].spec
		}
		place, err = e.network.Place(ctx, specs)
		if errors.Is(err, provider.ErrNoOffer) {
			return e.withoutOffer(exp, fmt.Sprintf("no place on the provider's private network for the job's %d nodes (%s)", len(ranks), shapesOf(shapes)))
		}
		if err != nil {
			return fmt.Errorf("cloudcompute: place the job's %d nodes: %w", len(ranks), err)
		}
	}
	rented, err := e.rent(ctx, job, ranks, place, v.unstarting)
	if err != nil {
		undone := e.undo(ctx, v, ranks, len(rented) == 0 && errors.Is(err, provider.ErrNoOffer))
		if errors.Is(err, provider.ErrNoOffer) && undone == nil {
			if len(rented) > 0 {
				return &agentexec.Unrunnable{Reason: clusterapi.PhaseReasonNoOffer, Why: fmt.Sprintf("place %s of the provider's private network had no machine left for every node of the job (%s) once its first node was rented, which was destroyed", place, shapesOf(shapes))}
			}
			return e.withoutOffer(exp, fmt.Sprintf("no machine to rent for shape %s within the provider's configured filters", shapesOf(shapes)))
		}
		return errors.Join(err, undone)
	}
	for _, i := range rented {
		e.log("created instance %s (%s): attempt %d of experiment %s is desired in slot %s", i.Name, i.ID, exp.AttemptCount, exp.ID, i.slot())
		v.add(i)
	}
	return nil
}

// shapesOf names the shapes of a job's ranks, each with how many ranks have it when that is more
// than one.
func shapesOf(shapes []Shape) string {
	var order []Shape
	count := map[string]int{}
	for _, s := range shapes {
		if count[s.Name] == 0 {
			order = append(order, s)
		}
		count[s.Name]++
	}
	named := make([]string, len(order))
	for n, s := range order {
		named[n] = fmt.Sprintf("%s (%s)", s.Name, cmp.Or(s.AcceleratorType, "no accelerator"))
		if count[s.Name] > 1 {
			named[n] = fmt.Sprintf("%d of %s", count[s.Name], named[n])
		}
	}
	return strings.Join(named, ", ")
}

// rent creates every rank in place, rank 0 first, whose address on the private network the others
// are told, and then the others at once, none on a host of avoid.
func (e *Executor) rent(ctx context.Context, job clusterapi.JobSpec, ranks []rank, place string, avoid []string) ([]instance, error) {
	first, err := e.rentRank(ctx, job, ranks[0], place, masterSelf, avoid)
	if err != nil {
		return nil, err
	}
	rented := []instance{first}
	if len(ranks) == 1 {
		return rented, nil
	}
	created, err := e.created(ctx, first)
	if err != nil {
		return rented, err
	}
	if created.PrivateAddress == "" {
		return rented, fmt.Errorf("cloudcompute: the provider created instance %s in place %s and shows no private address of it", first.Name, place)
	}
	master := created.PrivateAddress
	others := make([]instance, len(ranks))
	errs := make([]error, len(ranks))
	var wg sync.WaitGroup
	for r := 1; r < len(ranks); r++ {
		wg.Go(func() { others[r], errs[r] = e.rentRank(ctx, job, ranks[r], place, master, avoid) })
	}
	wg.Wait()
	for r := 1; r < len(ranks); r++ {
		if errs[r] == nil {
			rented = append(rented, others[r])
		}
	}
	return rented, errors.Join(errs...)
}

// created reads an instance this call rented, as the provider shows it.
func (e *Executor) created(ctx context.Context, i instance) (provider.Instance, error) {
	read, exists, err := e.provider.Get(ctx, i.ID)
	switch {
	case err != nil:
		return provider.Instance{}, fmt.Errorf("cloudcompute: read instance %s: %w", i.Name, err)
	case !exists:
		return provider.Instance{}, fmt.Errorf("cloudcompute: instance %s is gone right after it was created", i.Name)
	}
	return read, nil
}

// rentRank creates one rank, in place when it is one of several.
func (e *Executor) rentRank(ctx context.Context, job clusterapi.JobSpec, r rank, place, masterAddr string, avoid []string) (instance, error) {
	spec := r.spec
	if place != "" {
		spec = r.inPlace(job, place, masterAddr, e.network.Interface())
	}
	spec.AvoidHosts = avoid
	parsed, ok := e.parseInstanceName(spec.Name)
	if !ok {
		return instance{}, fmt.Errorf("cloudcompute: instance name %s does not read back as the rank it names", spec.Name)
	}
	id, err := e.provider.Create(ctx, spec)
	if err != nil {
		return instance{}, fmt.Errorf("cloudcompute: create instance %s: %w", spec.Name, err)
	}
	parsed.Instance = provider.Instance{ID: id, Name: spec.Name, State: provider.StateStarting}
	return parsed, nil
}

// undo destroys every instance the provider lists under the name of one of ranks, which a Create
// that failed may have left too. noneLeft says nothing can be listed, as when only rank 0 was asked
// for and the provider had no offer.
func (e *Executor) undo(ctx context.Context, v *view, ranks []rank, noneLeft bool) error {
	if noneLeft {
		return nil
	}
	names := make([]string, len(ranks))
	for r := range ranks {
		names[r] = ranks[r].spec.Name
	}
	listed, err := e.provider.List(ctx, e.prefix)
	if err != nil {
		return fmt.Errorf("cloudcompute: list instances to undo a partial rental: %w", err)
	}
	var errs []error
	for _, l := range listed {
		if !slices.Contains(names, l.Name) {
			continue
		}
		if err := e.destroy(ctx, v, instance{Instance: l}, "not every rank of its job could be rented"); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// withoutOffer is what a job is told when there is no machine, or no place, to rent for it:
// that it waits, and once it has waited OfferWait since its admission, that this cluster has
// none for it. The control plane then queues the job again, where it waits for slots that have
// an offer.
func (e *Executor) withoutOffer(exp *clusterapi.DesiredJob, what string) error {
	if exp.SubmittedAt == nil {
		return fmt.Errorf("cloudcompute: experiment %s is placed here with no admission time", exp.ID)
	}
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
				errs = append(errs, fmt.Errorf("cloudcompute: stop instance %s: %w", i.Name, err))
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
		return fmt.Errorf("cloudcompute: destroy instance %s (%s): %s: %w", i.Name, i.ID, why, err)
	}
	e.log("destroyed instance %s (%s): %s", i.Name, i.ID, why)
	v.remove(i.ID)
	return nil
}

// Leave is what the process does last, after its loop has ended: it says by name what is still
// rented under this namespace, which the next start of this runtime finds in the vendor's list
// and reconciles, a destroy that failed among it.
func (e *Executor) Leave(ctx context.Context) error {
	listed, err := e.provider.List(ctx, e.prefix)
	if err != nil {
		return fmt.Errorf("cloudcompute: what is still rented could not be listed: %w", err)
	}
	for _, i := range listed {
		e.log("leaving instance %s (%s) rented, %s", i.Name, i.ID, i.State)
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

// WorkloadMatchesDesired is true when every live instance of exp is its workload (workloadOf) and
// no rank is missing, since the other ranks of a gang wait for a missing one forever. An instance
// overdue to start does not match, and one the provider does not know to be running does, for the
// control plane to bound. The workload of a desired job in a slot no longer configured is an error
// and is left running, as destroying it would end a job the control plane still wants.
func (e *Executor) WorkloadMatchesDesired(ctx context.Context, exp *clusterapi.DesiredJob) (bool, error) {
	v, err := e.current(ctx)
	if err != nil {
		return false, err
	}
	matches := false
	present := map[int]bool{}
	for _, i := range v.of(exp.ID) {
		if workloadOf(exp, i) {
			present[i.rank] = true
		}
		if !i.live() {
			continue
		}
		if !workloadOf(exp, i) {
			return false, nil
		}
		if _, _, ok := e.configured(i.slot()); !ok {
			return false, fmt.Errorf("cloudcompute: instance %s runs experiment %s in slot %s, which no configured shape has, so it is billed beside every slot: restore the shape or its max_instances, or end the job", i.Name, exp.ID, i.slot())
		}
		if e.overdue(i) {
			e.log("instance %s has not started in %s and is replaced with every rank of its job", i.Name, e.startup)
			return false, nil
		}
		matches = true
	}
	if ranks := len(exp.EffectiveJob().Slots); matches && len(present) < ranks {
		e.log("experiment %s has %d of its %d ranks and is rented again whole", exp.ID, len(present), ranks)
		return false, nil
	}
	return matches, nil
}

// PollJobStatus reports the ranks of experimentID's newest attempt as one job, under rank 0's name
// as its UID and on rank 0's machine at the price of every rank. Any failed rank fails it; else a
// missing rank or one that exited without the wrapper's exit line, unread output included, makes
// it gone, which the control plane confirms over several snapshots and requeues as an
// infrastructure fault. It has succeeded once every rank has, and runs while some rank runs and
// every other has succeeded; anything else is pending, which the control plane bounds. A poll never
// fails for one instance, as that would withhold the cluster's whole status push. When the command
// ran is the wrapper's word, from the first rank's start to the last rank's end.
func (e *Executor) PollJobStatus(ctx context.Context, experimentID string) (agentexec.JobStatus, error) {
	v, err := e.current(ctx)
	if err != nil {
		return agentexec.JobStatus{Phase: clusterapi.JobPhasePending, Attempt: clusterapi.AttemptUnknown}, err
	}
	ranks := v.gang(experimentID)
	if len(ranks) == 0 {
		return agentexec.JobStatus{Phase: clusterapi.JobPhaseGone, Attempt: clusterapi.AttemptUnknown}, nil
	}
	head := ranks[0]
	observed := agentexec.JobStatus{Phase: clusterapi.JobPhasePending, UID: head.Name, Attempt: head.attempt, Machine: &clusterapi.JobMachine{
		InstanceID: head.ID, HostID: head.Host, AcceleratorModel: head.AcceleratorModel, CPUModel: head.CPUModel, PricePerHourUSD: head.PricePerHour,
	}}
	failed, gone, succeeded, running := false, len(ranks) < head.ranks, 0, 0
	for _, i := range ranks {
		if i.ID != head.ID {
			observed.Machine.PricePerHourUSD += i.PricePerHour
		}
		switch i.State {
		case provider.StateRunning:
			running++
		case provider.StateExited:
			how, _, readErr := e.ending(ctx, v, i)
			if readErr != nil {
				e.log("reporting %s as gone: %v", i.Name, readErr)
			}
			failed = failed || how == endingFailed
			gone = gone || how == endingUnknown
			if how == endingSucceeded {
				succeeded++
			}
			if how != endingUnknown {
				if line, found := clusterapi.FindExitLine(e.output(ctx, v, i).lines); found {
					if observed.StartedAt.IsZero() || line.StartedAt.Before(observed.StartedAt) {
						observed.StartedAt = line.StartedAt
					}
					if line.EndedAt.After(observed.EndedAt) {
						observed.EndedAt = line.EndedAt
					}
				}
			}
		}
	}
	switch {
	case failed:
		observed.Phase = clusterapi.JobPhaseFailed
	case gone:
		observed.Phase = clusterapi.JobPhaseGone
	case succeeded == head.ranks:
		observed.Phase = clusterapi.JobPhaseSucceeded
	case running > 0 && running+succeeded == head.ranks:
		observed.Phase = clusterapi.JobPhaseRunning
	}
	if observed.Phase != clusterapi.JobPhaseFailed && observed.Phase != clusterapi.JobPhaseSucceeded {
		observed.StartedAt, observed.EndedAt = time.Time{}, time.Time{}
	}
	return observed, nil
}

// ResolveAdmittedAcceleratorType reports the type and node of the slot rank 0 of experimentID's
// newest attempt occupies: the type its units are priced under (Shape.units), cores for a shape
// with none. Without rank 0 it names no node, as the control plane fences a report on the slot
// of a job's first node.
func (e *Executor) ResolveAdmittedAcceleratorType(ctx context.Context, experimentID string) (clusterapi.AcceleratorType, string, bool, error) {
	v, err := e.current(ctx)
	if err != nil {
		return "", "", true, err
	}
	ranks := v.gang(experimentID)
	if len(ranks) == 0 || ranks[0].rank != 0 {
		return "", "", true, nil
	}
	shape, _, _ := e.configured(ranks[0].slot())
	unit, _ := shape.units()
	return clusterapi.AcceleratorType(unit), ranks[0].slot(), true, nil
}

// ReapTerminal destroys every instance of one name beyond the lowest id, which status reads
// report, every exited instance that is not a desired experiment's workload, and every instance
// under the prefix whose name is no job's.
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
