package podexec

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/moby/moby/client"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/workload"
)

// CreateWorkload places exp's container on this node. No admission handshake: the scheduler
// loop already decided this experiment fits before calling this, matching k8sexec.CreateWorkload.
func (e *Executor) CreateWorkload(ctx context.Context, exp *domain.Experiment) error {
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
		// Drifted: remove the stale container (whatever state it's in) so this call can
		// recreate it fresh, mirroring k8sexec's delete-then-error-for-retry pattern.
		if err := e.removeContainer(ctx, c.name()); err != nil {
			return fmt.Errorf("podexec: remove drifted container %s: %w", c.name(), err)
		}
		return fmt.Errorf("podexec: stale container for %s removed; retry creation", exp.ID)
	}
	if err := e.startContainer(ctx, spec); err != nil {
		return fmt.Errorf("podexec: start container for %s: %w", exp.ID, err)
	}
	return nil
}

// DeleteWorkload stops (but does not remove) exp's container — the container that remains
// stopped IS the "gone" record until pruneTerminal removes it (different-backend.md §4.6).
// A naive remove-on-delete would mean WaitForJobDeletion / status reporting could never observe
// a "gone" phase for a container that no longer exists to be listed.
func (e *Executor) DeleteWorkload(ctx context.Context, experimentID string, grantCheckpointWindow bool) error {
	containers, err := e.listManagedContainers(ctx)
	if err != nil {
		return err
	}
	for _, c := range containers {
		if c.Labels[LabelExperimentID] != experimentID {
			continue
		}
		return e.stopContainer(ctx, c.name(), e.stopGrace(c.Labels, grantCheckpointWindow))
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
func (e *Executor) stopGrace(labels map[string]string, grantCheckpointWindow bool) int64 {
	grace := e.defaultTerminationGracePeriodSeconds
	if g, err := strconv.ParseInt(labels[LabelGraceSeconds], 10, 64); err == nil && g > 0 {
		grace = g
	}
	if !grantCheckpointWindow {
		return grace
	}
	if w, err := strconv.ParseInt(labels[LabelCheckpointGrace], 10, 64); err == nil && w > grace {
		grace = w
	}
	return grace
}

// WaitForJobDeletion blocks until experimentID's container reports JobPhaseGone (stopped and no
// longer desired) or timeout elapses — the workload.Backend contract's confirmation hook for
// preemption/eviction.
func (e *Executor) WaitForJobDeletion(ctx context.Context, experimentID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		phase, _, err := e.PollJobPhaseAndUID(ctx, experimentID)
		if err != nil {
			return err
		}
		if phase == workload.JobPhaseGone || phase == workload.JobPhaseSucceeded || phase == workload.JobPhaseFailed {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("podexec: timed out waiting for %s to stop", experimentID)
}

// ListManagedJobs returns the experiment IDs of every container this executor manages that is
// still running or pending — used by the reconcile loop to diff actual vs desired. A container
// already stopped is not "actual" (its job no longer occupies capacity), matching k8sexec
// excluding Jobs mid-teardown.
func (e *Executor) ListManagedJobs(ctx context.Context) ([]string, error) {
	containers, err := e.listManagedContainers(ctx)
	if err != nil {
		return nil, err
	}
	var ids []string
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
		ids = append(ids, id)
	}
	return ids, nil
}

// ListManagedJobsForStatus returns every managed experiment ID regardless of container state —
// unlike ListManagedJobs (which only answers "does the desired container still need creating?"
// and so excludes exited containers), status reporting must see an exited container at least
// once so its terminal phase (PollJobPhaseAndUID's Succeeded/Failed) reaches the control plane
// before reapTerminal prunes it.
func (e *Executor) ListManagedJobsForStatus(ctx context.Context) ([]string, error) {
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
		ids = append(ids, id)
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

// PollJobPhase reports experimentID's current lifecycle phase.
func (e *Executor) PollJobPhase(ctx context.Context, experimentID string) (workload.JobPhase, error) {
	phase, _, err := e.PollJobPhaseAndUID(ctx, experimentID)
	return phase, err
}

// PollJobPhaseAndUID inspects experimentID's container state. The container ID itself stands
// in for k8s's UID: it changes on every stop+remove+recreate cycle, so callers can still detect
// a preempt-then-recreate race the same way status.go's reportChangedStatuses does.
func (e *Executor) PollJobPhaseAndUID(ctx context.Context, experimentID string) (workload.JobPhase, string, error) {
	containers, err := e.listManagedContainers(ctx)
	if err != nil {
		return workload.JobPhasePending, "", err
	}
	for _, c := range containers {
		if c.Labels[LabelExperimentID] != experimentID {
			continue
		}
		switch c.State {
		case "running":
			return workload.JobPhaseRunning, c.ID, nil
		case "created":
			// Created but not started is not running: it consumes nothing and may never start.
			// Reporting it as running billed a wedged container as if it were training.
			return workload.JobPhasePending, c.ID, nil
		case "exited", "stopped":
			code, err := e.exitCode(ctx, c.ID)
			if err != nil {
				return workload.JobPhasePending, "", err
			}
			if code == 0 {
				return workload.JobPhaseSucceeded, c.ID, nil
			}
			return workload.JobPhaseFailed, c.ID, nil
		default:
			return workload.JobPhasePending, c.ID, nil
		}
	}
	return workload.JobPhaseGone, "", nil
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

func (e *Executor) GetAdmittedAcceleratorType(ctx context.Context, exp *domain.Experiment) (domain.AcceleratorType, error) {
	t, _, _, err := e.ResolveAdmittedAcceleratorType(ctx, exp.ID)
	if err != nil {
		return "", err
	}
	if t == "" {
		return "", fmt.Errorf("podexec: accelerator type for %s is not observed yet", exp.ID)
	}
	return t, nil
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
func (e *Executor) reapTerminal(ctx context.Context, desiredIDs map[string]bool) error {
	containers, err := e.listManagedContainers(ctx)
	if err != nil {
		return err
	}
	for _, c := range containers {
		if c.State == "running" || c.State == "created" {
			continue
		}
		if desiredIDs[c.Labels[LabelExperimentID]] {
			continue
		}
		if err := e.removeContainer(ctx, c.name()); err != nil {
			// Finish the pass: one container that refuses to be removed must not leave every
			// other terminal container on the node un-reaped (important.md #19).
			log.Printf("podexec: reap terminal container %s: %v", c.name(), err)
			continue
		}
	}
	return nil
}
