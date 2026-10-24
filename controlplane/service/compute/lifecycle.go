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

// workloadOf reports whether i is exp's workload: an instance of its current attempt, one of its
// ranks, in the slot admission pinned that rank to. The attempt alone does not say so. A
// preempted job is readmitted under the attempt it had, and the instance it left in another slot
// would otherwise pass for it, and that instance's exit for the job's result.
func workloadOf(exp *clusterapi.DesiredJob, i instance) bool {
	slots := exp.EffectiveJob().Slots
	return i.experimentID == exp.ID && i.attempt == exp.AttemptCount && i.ranks == len(slots) && i.slot() == slots[i.rank]
}

// masterSelf is the address rank 0 of a job that spans several nodes is told it meets the others
// at: its own, which it listens on, since the address the others reach it at is known only once
// it is rented.
const masterSelf = "127.0.0.1"

// CreateWorkload rents the instances of exp's current attempt, one per rank, each in the slot
// admission placed that rank on. It makes no placement decision of its own: desired state names
// the slots, and a slot that cannot take its rank is an error, never a reason to pick another.
//
// The instances of that attempt in those slots, in any state, are the workload: one that has
// exited is waiting for the control plane to read its outcome, and renting another would run the
// job twice. Any other instance of the experiment is left over from an earlier attempt or an
// earlier placement. An exited one is destroyed before the new instances are rented, so a status
// read never finds two placements of one job to choose between. A live one is the reconcile
// pass's to delete first, and until it has, nothing is rented.
//
// A job of several ranks is rented whole or not at all: on a private network made for its
// attempt, rank 0 first, whose address the others are told, then the others at once. Whatever
// fails, everything this call rented is destroyed and the network deleted before it returns, so
// nothing half-rented outlives the pass.
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
	network := ""
	if len(ranks) > 1 {
		network = e.networkName(exp.ID, exp.AttemptCount)
		specs := make([]provider.Spec, len(ranks))
		for r := range ranks {
			specs[r] = ranks[r].spec
		}
		err := e.network.CreateNetwork(ctx, network, specs, spreads(job))
		if errors.Is(err, provider.ErrNoOffer) {
			return e.withoutOffer(exp, fmt.Sprintf("no place to make private network %s in for the job's %d nodes (%s)", network, len(ranks), shapesOf(shapes)))
		}
		if err != nil {
			return fmt.Errorf("cloudcompute: create network %s: %w", network, err)
		}
	}
	rented, err := e.rent(ctx, job, ranks, network, v.unstarting)
	if err != nil {
		undone := e.undo(ctx, v, rented, network)
		if errors.Is(err, provider.ErrNoOffer) && undone == nil {
			what := fmt.Sprintf("no machine to rent for shape %s within the provider's configured filters", shapesOf(shapes))
			if network != "" {
				what = fmt.Sprintf("no machine to rent for every node of the job (%s) on private network %s", shapesOf(shapes), network)
			}
			return e.withoutOffer(exp, what)
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

// rent creates every rank, rank 0 first, and returns what it rented, all of them when it returns
// no error. A rank on a network is told rank 0's address on it. None is rented on a host of
// avoid. When the job spreads, the others are rented one after another, each on a host no rank
// before it has, and a rank whose host the provider does not name fails the create, as nothing
// then keeps the next off it; otherwise they are rented at once.
func (e *Executor) rent(ctx context.Context, job clusterapi.JobSpec, ranks []rank, network string, avoid []string) ([]instance, error) {
	first, err := e.rentRank(ctx, job, ranks[0], network, masterSelf, avoid)
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
		return rented, fmt.Errorf("cloudcompute: the provider created instance %s on network %s and shows no address of it there", first.Name, network)
	}
	master := created.PrivateAddress
	if !spreads(job) {
		others := make([]instance, len(ranks))
		errs := make([]error, len(ranks))
		var wg sync.WaitGroup
		for r := 1; r < len(ranks); r++ {
			wg.Go(func() { others[r], errs[r] = e.rentRank(ctx, job, ranks[r], network, master, avoid) })
		}
		wg.Wait()
		for r := 1; r < len(ranks); r++ {
			if errs[r] == nil {
				rented = append(rented, others[r])
			}
		}
		return rented, errors.Join(errs...)
	}
	avoid = slices.Clone(avoid)
	for r := 1; ; r++ {
		if created.Host == "" {
			return rented, fmt.Errorf("cloudcompute: the provider names no host of instance %s, so the job's nodes cannot be kept on hosts of their own", rented[r-1].Name)
		}
		if r == len(ranks) {
			return rented, nil
		}
		avoid = append(avoid, created.Host)
		i, err := e.rentRank(ctx, job, ranks[r], network, master, avoid)
		if err != nil {
			return rented, err
		}
		rented = append(rented, i)
		if created, err = e.created(ctx, i); err != nil {
			return rented, err
		}
	}
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

// rentRank creates one rank, on network when it is one of several.
func (e *Executor) rentRank(ctx context.Context, job clusterapi.JobSpec, r rank, network, masterAddr string, avoid []string) (instance, error) {
	spec := r.spec
	if network != "" {
		spec = r.onNetwork(job, network, masterAddr, e.network.Interface())
	}
	spec.AvoidHosts = avoid
	id, err := e.provider.Create(ctx, spec)
	if err != nil {
		return instance{}, fmt.Errorf("cloudcompute: create instance %s: %w", spec.Name, err)
	}
	parsed, _ := e.parseInstanceName(spec.Name)
	parsed.Instance = provider.Instance{ID: id, Name: spec.Name, State: provider.StateStarting}
	return parsed, nil
}

// undo destroys what a CreateWorkload rented and deletes its network, once no instance is on it.
func (e *Executor) undo(ctx context.Context, v *view, rented []instance, network string) error {
	var errs []error
	for _, i := range rented {
		if err := e.destroy(ctx, v, i, "not every rank of its job could be rented"); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 || network == "" {
		return errors.Join(errs...)
	}
	return e.deleteNetwork(ctx, network, "not every rank of its job could be rented")
}

func (e *Executor) deleteNetwork(ctx context.Context, name, why string) error {
	if err := e.network.DeleteNetwork(ctx, name); err != nil {
		return fmt.Errorf("cloudcompute: delete network %s: %s: %w", name, why, err)
	}
	e.log("deleted network %s: %s", name, why)
	return nil
}

// withoutOffer is what a job is told when there is no machine, or no network, to rent for it:
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

// WorkloadMatchesDesired is true when every live instance of exp is its workload, of its current
// attempt and each rank in the slot it is pinned to, and no rank of it is missing. Attempt, rank
// and slot are in the name, which is all an instance can be compared on. A requeue changes the
// first, a readmission the last. A rank that vanished leaves the others waiting for it in a
// rendezvous or a collective they never leave: the pass deletes every rank and the next rents the
// whole job again.
//
// An instance that has not started within Startup does not match either: the pass destroys it
// with the rest of its job and the next rents them again, until the control plane's own deadline
// for a job that is not running requeues or ends the job.
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

// PollJobStatus reports the ranks of experimentID's newest attempt as one job. The name of rank 0
// stands in for a UID: it differs between attempts and between slots.
//
// The ranks combine as a gang does on any runtime: any rank that failed fails the job; else any
// rank that exited without the wrapper's exit line, or that is missing, makes it gone; it has
// succeeded once every rank has, and runs while some rank runs and every other has succeeded, as
// a gang on k8s runs while any pod is ready. Anything else is pending. The machine is rank 0's,
// at the price of every rank together.
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
// command ran is therefore the wrapper's word, read from the output with the exit line: from the
// first rank's start to the last rank's end.
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

// ReapTerminal destroys what no pass has a use for any more: every instance of one name beyond
// the first, every exited instance that is not the workload of a desired experiment, which is a
// job the control plane has finished with, or what a requeue or a readmission to another slot
// left behind, and every instance under this namespace's prefix whose name is no job's. One
// failed destroy does not keep the rest rented.
//
// A second instance of one name is a create the vendor carried out and reported failed, or a
// list that lagged behind a create. The lowest id is kept, which is also the one every status
// read reports, so which of the two survives does not depend on the pass that finds them.
//
// Networks go after the instances, as a vendor may end whatever is still on a network it deletes.
// A network is deleted once no instance of its attempt is left and it is not that of a desired
// job's current attempt, whose ranks may be rented on it yet.
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
	// An instance whose name is no job's may be on any network.
	if e.network != nil && len(v.unnamed) == 0 {
		errs = append(errs, e.reapNetworks(ctx, v, desired))
	}
	return errors.Join(errs...)
}

func (e *Executor) reapNetworks(ctx context.Context, v *view, desired map[string]*clusterapi.DesiredJob) error {
	names, err := e.network.Networks(ctx, e.networkPrefix)
	if err != nil {
		return fmt.Errorf("cloudcompute: list networks: %w", err)
	}
	var errs []error
	for _, name := range names {
		experimentID, attempt, ok := e.parseNetworkName(name)
		if ok && slices.ContainsFunc(v.of(experimentID), func(i instance) bool { return i.attempt == attempt }) {
			continue
		}
		if exp := desired[experimentID]; ok && exp != nil && exp.AttemptCount == attempt {
			continue
		}
		if err := e.deleteNetwork(ctx, name, "no instance is on it and no desired job's attempt is"); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
