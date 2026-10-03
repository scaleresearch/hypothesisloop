package podexec

import (
	"context"
	"fmt"
	"log"
	"slices"
	"strconv"

	"github.com/moby/moby/client"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/workload"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/agentexec"
)

// CreateWorkload places exp's container on this node. No admission handshake: the scheduler
// loop already decided this experiment fits before calling this, matching k8sexec.CreateWorkload.
//
// The devices are chosen here, in the pass. The pull and start then run in their own goroutine
// under ctx, so a slow pull holds up no other job; a failure is logged and the next pass, finding
// the job still without a container, tries again. How the pull ended is remembered: a pass that
// finds the image of this very spec and attempt can never be pulled starts no further pull and
// returns the job as unrunnable with what the engine said.
func (e *Executor) CreateWorkload(ctx context.Context, exp *domain.Experiment) error {
	if _, busy := e.operations()[exp.ID]; busy {
		return nil
	}
	placement, err := e.resolvePlacementFor(ctx, exp)
	if err != nil {
		return err
	}
	spec, err := e.BuildContainerSpec(exp, placement)
	if err != nil {
		return err
	}
	existing, err := e.listManagedContainers(ctx)
	if err != nil {
		return err
	}
	for _, c := range existing {
		if c.Labels[LabelExperimentID] != exp.ID {
			continue
		}
		if c.Labels[LabelDesiredSpecHash] == spec.Labels[LabelDesiredSpecHash] {
			if c.State == "created" {
				// Created but never started — the container exists and hash-matches, so this
				// used to read as "already running" and the start was never retried, wedging the
				// job silently for its whole life.
				if _, err := e.docker.ContainerStart(ctx, c.ID, client.ContainerStartOptions{}); err != nil {
					return fmt.Errorf("podexec: start created container %s: %w", c.name(), err)
				}
			}
			return nil // already running the desired spec
		}
		// Drifted: remove the stale container (whatever state it's in). A later pass, finding
		// it gone, creates the desired one.
		name := c.name()
		e.begin(ctx, exp.ID, "remove drifted container of", operation{}, func(ctx context.Context) error {
			return e.removeContainer(ctx, name)
		})
		return nil
	}
	if failed, ok := e.pullFailures()[exp.ID]; ok && failed.reason != "" &&
		failed.labels[LabelDesiredSpecHash] == spec.Labels[LabelDesiredSpecHash] && failed.labels[LabelAttempt] == spec.Labels[LabelAttempt] {
		return &agentexec.Unrunnable{Reason: failed.reason, Why: failed.message}
	}
	e.begin(ctx, exp.ID, "create workload", operation{starting: true, labels: spec.Labels, devices: placement.DevicePaths}, func(ctx context.Context) error {
		err := e.pullImage(ctx, spec.Image)
		e.recordPull(ctx, exp.ID, spec.Labels, err)
		if err != nil {
			return err
		}
		return e.startContainer(ctx, spec)
	})
	return nil
}

// DeleteWorkload stops (but does not remove) exp's container — the container that remains
// stopped IS the "gone" record until pruneTerminal removes it (different-backend.md §4.6).
// A naive remove-on-delete would mean status reporting could never observe
// a "gone" phase for a container that no longer exists to be listed.
//
// The stop runs in its own goroutine under ctx and may take the whole grace. Until it returns the
// job keeps its requests and devices, whatever state the engine reports for a container that is
// on its way down. A job still pulling has its pull cancelled instead; if a container came up
// anyway, a later pass finds it and stops it here.
func (e *Executor) DeleteWorkload(ctx context.Context, experimentID string, grantCheckpointWindow bool) error {
	if op, busy := e.operations()[experimentID]; busy {
		if op.starting {
			op.cancel()
		}
		return nil
	}
	containers, err := e.listManagedContainers(ctx)
	if err != nil {
		return err
	}
	for _, c := range containers {
		if c.Labels[LabelExperimentID] != experimentID {
			continue
		}
		name := c.name()
		// A container that cannot say its grace is still stopped, or it holds its device for
		// good. The engine then decides the grace (nil), and the delete is an error saying so.
		var grace *int
		seconds, graceErr := stopGrace(c.Labels, grantCheckpointWindow)
		if graceErr == nil {
			timeout := int(seconds)
			grace = &timeout
		}
		devices, err := e.currentDevicesFor(ctx, experimentID)
		if err != nil {
			return err
		}
		e.begin(ctx, experimentID, "delete workload", operation{labels: c.Labels, devices: devices}, func(ctx context.Context) error {
			return e.stopContainer(ctx, name, grace)
		})
		if graceErr != nil {
			return fmt.Errorf("podexec: container %s is being stopped on the engine's own grace, its job's is lost: %w", name, graceErr)
		}
		return nil
	}
	return nil // nothing to delete
}

// stopGrace is how long this container gets between SIGTERM and SIGKILL for this stop: the
// checkpoint window it declared when the control plane granted this termination that window, and
// the ordinary shutdown grace otherwise.
//
// A granted window only ever lengthens the grace. Everything else -- an infrastructure or
// workload failure, a drift replacement -- stops on the ordinary grace whatever the job
// declared, because there is nothing to save or nothing left to save it with.
//
// A container with no readable grace is an error rather than a guess.
func stopGrace(labels map[string]string, grantCheckpointWindow bool) (int64, error) {
	grace, err := strconv.ParseInt(labels[LabelGraceSeconds], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("no readable shutdown grace: %w", err)
	}
	if !grantCheckpointWindow {
		return grace, nil
	}
	window, err := strconv.ParseInt(labels[LabelCheckpointGrace], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("no readable checkpoint grace: %w", err)
	}
	return max(grace, window), nil
}

// ListManagedJobs returns the experiment IDs of every container this executor manages that is
// still running or pending — used by the reconcile loop to diff actual vs desired. A container
// already stopped is not "actual" (its job no longer occupies capacity), matching k8sexec
// excluding Jobs mid-teardown.
//
// A job with an operation in flight is listed from the operation alone: one that is pulling is
// actual, so the pass compares it with desired state and deletes it when it is no longer wanted,
// and one being stopped or removed is left out, so the pass starts nothing more for it.
func (e *Executor) ListManagedJobs(ctx context.Context) ([]string, error) {
	ops := e.operations()
	containers, err := e.listManagedContainers(ctx)
	if err != nil {
		return nil, err
	}
	var ids []string
	for id, op := range ops {
		if op.starting {
			ids = append(ids, id)
		}
	}
	for _, c := range containers {
		if c.State != "running" && c.State != "created" {
			continue
		}
		id := c.Labels[LabelExperimentID]
		if id == "" {
			// One unidentifiable container must not brick the node: this list drives both
			// reconcile and status reporting (important.md #19).
			log.Printf("podexec: skipping managed container %q with no experiment identity", c.name())
			continue
		}
		if _, busy := ops[id]; busy {
			continue
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// ListManagedJobsForStatus returns every managed experiment ID regardless of container state —
// unlike ListManagedJobs (which only answers "does the desired container still need creating?"
// and so excludes exited containers), status reporting must see an exited container at least
// once so its terminal phase (PollJobStatus's Succeeded/Failed) reaches the control plane
// before reapTerminal prunes it.
//
// A job whose image is still being pulled has no container and is listed from its operation, so
// the control plane hears it is pending instead of reading its absence as gone. So is a job
// between a failed pull and its retry, listed from the failure.
func (e *Executor) ListManagedJobsForStatus(ctx context.Context) ([]string, error) {
	ops := e.operations()
	containers, err := e.listManagedContainers(ctx)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, c := range containers {
		id := c.Labels[LabelExperimentID]
		if id == "" {
			log.Printf("podexec: skipping managed container %q with no experiment identity", c.name())
			continue
		}
		delete(ops, id)
		ids = append(ids, id)
	}
	for id, op := range ops {
		if op.starting {
			ids = append(ids, id)
		}
	}
	for id, failed := range e.pullFailures() {
		if failed.reason == "" && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// ListManagedAuxiliaryWorkloads mirrors k8sexec's call shape (orphan discovery) but this
// runtime creates no auxiliary resources (no headless Service, no DRA template — single-node,
// no distributed rendezvous), so there is nothing to ever orphan.
func (e *Executor) ListManagedAuxiliaryWorkloads(_ context.Context) ([]string, error) {
	return nil, nil
}

// WorkloadMatchesDesired recompiles exp and compares its hash against the currently running
// container's recorded desired-spec hash.
func (e *Executor) WorkloadMatchesDesired(ctx context.Context, exp *domain.Experiment) (bool, error) {
	placement, err := e.resolvePlacementFor(ctx, exp)
	if err != nil {
		return false, err
	}
	desired, err := e.BuildContainerSpec(exp, placement)
	if err != nil {
		return false, err
	}
	if op, busy := e.operations()[exp.ID]; busy && op.starting {
		return op.labels[LabelDesiredSpecHash] == desired.Labels[LabelDesiredSpecHash], nil
	}
	containers, err := e.listManagedContainers(ctx)
	if err != nil {
		return false, err
	}
	for _, c := range containers {
		if c.Labels[LabelExperimentID] != exp.ID {
			continue
		}
		return c.Labels[LabelDesiredSpecHash] == desired.Labels[LabelDesiredSpecHash], nil
	}
	return false, nil
}

// PollJobStatus inspects experimentID's container state. The container ID itself stands
// in for k8s's UID: it changes on every stop+remove+recreate cycle, so callers can still detect
// a preempt-then-recreate race the same way status.go's reportChangedStatuses does.
func (e *Executor) PollJobStatus(ctx context.Context, experimentID string) (agentexec.JobStatus, error) {
	pending := agentexec.JobStatus{Phase: workload.JobPhasePending, Attempt: workload.AttemptUnknown}
	op, busy := e.operations()[experimentID]
	containers, err := e.listManagedContainers(ctx)
	if err != nil {
		return pending, err
	}
	for _, c := range containers {
		if c.Labels[LabelExperimentID] != experimentID {
			continue
		}
		// Unparseable or absent reads as unknown rather than as a number: the control plane
		// fences status on this, and a wrong generation is worse than no generation.
		observed := agentexec.JobStatus{UID: c.ID, Attempt: attemptOf(c.Labels)}
		switch c.State {
		case "running":
			observed.Phase = workload.JobPhaseRunning
		case "created":
			// Created but not started is not running: it consumes nothing and may never start.
			// Reporting it as running billed a wedged container as if it were training.
			observed.Phase = workload.JobPhasePending
		case "exited", "stopped":
			code, reported, err := e.exitCode(ctx, c.ID)
			if err != nil {
				return pending, err
			}
			// An exit nobody reported is not a success. PollPhaseDetail says why it is a failure.
			observed.Phase = workload.JobPhaseFailed
			if reported && code == 0 {
				observed.Phase = workload.JobPhaseSucceeded
			}
		default:
			observed.Phase = workload.JobPhasePending
		}
		return observed, nil
	}
	if busy && op.starting {
		pending.Attempt = attemptOf(op.labels)
		return pending, nil
	}
	if failed, ok := e.pullFailures()[experimentID]; ok && failed.reason == "" {
		pending.Attempt = attemptOf(failed.labels)
		return pending, nil
	}
	return agentexec.JobStatus{Phase: workload.JobPhaseGone, Attempt: workload.AttemptUnknown}, nil
}

func attemptOf(labels map[string]string) int {
	n, err := strconv.Atoi(labels[LabelAttempt])
	if err != nil {
		return workload.AttemptUnknown
	}
	return n
}

// ResolveAdmittedAcceleratorType reports the accelerator type and node this experiment actually
// ran on. On a single bare node there is no scheduler-side substitution to observe (this
// runtime resolves placement itself, deterministically, in resolvePlacementFor) — this simply
// reads back what was recorded on the container at creation time, and node is always this
// node's own identity.
func (e *Executor) ResolveAdmittedAcceleratorType(ctx context.Context, experimentID string) (domain.AcceleratorType, string, bool, error) {
	containers, err := e.listManagedContainers(ctx)
	if err != nil {
		return "", "", true, err
	}
	for _, c := range containers {
		if c.Labels[LabelExperimentID] != experimentID {
			continue
		}
		if c.State != "running" && c.State != "created" {
			return "", "", true, nil
		}
		return domain.AcceleratorType(c.Labels[LabelAcceleratorType]), e.nodeName, true, nil
	}
	return "", "", true, nil
}

// ReapTerminal removes containers that are stopped and whose experiment ID is not a key of
// desired — the exported entry point reconcile.go calls once per tick.
func (e *Executor) ReapTerminal(ctx context.Context, desired map[string]*domain.Experiment) error {
	desiredIDs := make(map[string]bool, len(desired))
	for id := range desired {
		desiredIDs[id] = true
	}
	return e.reapTerminal(ctx, desiredIDs)
}

// reapTerminal removes containers that are both stopped and no longer desired
// (different-backend.md §4.6: the stopped container is the "gone" record right up until it's
// pruned). No retained cleanup queue — every tick recomputes purely from currently-listed
// containers, so a restart of this process loses nothing.
//
// A job with an operation in flight is skipped: an engine can report a container as neither
// running nor created while its stop is still inside the grace, and removing it then would kill a
// job in the middle of its checkpoint.
func (e *Executor) reapTerminal(ctx context.Context, desiredIDs map[string]bool) error {
	e.forgetPullsExcept(desiredIDs)
	ops := e.operations()
	containers, err := e.listManagedContainers(ctx)
	if err != nil {
		return err
	}
	for _, c := range containers {
		if c.State == "running" || c.State == "created" {
			continue
		}
		id := c.Labels[LabelExperimentID]
		if desiredIDs[id] {
			continue
		}
		if _, busy := ops[id]; busy {
			continue
		}
		name := c.name()
		e.begin(ctx, id, "reap terminal container of", operation{}, func(ctx context.Context) error {
			return e.removeContainer(ctx, name)
		})
	}
	return nil
}
