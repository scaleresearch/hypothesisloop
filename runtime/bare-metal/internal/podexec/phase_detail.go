package podexec

import (
	"context"
	"fmt"
	"strings"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/client"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
)

// PollPhaseDetail inspects experimentID's container (if one exists yet) for an error and
// restart count. Read live on every call — no caching (important.md #4). Returns empty
// reason/message and nil error when there is nothing notable to report.
//
// A job whose image is being pulled reports what a k8s pod in ContainerCreating does: it has
// landed on this node (scheduledNodes 1) and has no reason yet. Once a pull has failed and is
// being retried, the message is what the engine said of the last one, until a pull succeeds or
// the stuck-pending timeout ends the wait. An image that can never be pulled is not reported
// here: CreateWorkload returns that job as unrunnable.
func (e *Executor) PollPhaseDetail(ctx context.Context, experimentID string, _ int) (reason, message string, restartCount int32, scheduledNodes int32, err error) {
	op, busy := e.operations()[experimentID]
	containers, err := e.listManagedContainers(ctx)
	if err != nil {
		return "", "", 0, 0, err
	}
	for _, c := range containers {
		if c.Labels[LabelExperimentID] != experimentID {
			continue
		}
		resp, err := e.docker.ContainerInspect(ctx, c.ID, client.ContainerInspectOptions{})
		if err != nil {
			return "", "", 0, 0, fmt.Errorf("podexec: inspect container %s: %w", c.ID, err)
		}
		restartCount = int32(resp.Container.RestartCount)
		// Bare metal has no autoscaler and no gang concept -- one experiment, one container, on a
		// host that either exists or doesn't. scheduledNodes reports 1 once the container exists
		// (Docker created it, it has landed), matching how a k8s pod that has bound is counted.
		scheduledNodes = 1
		if resp.Container.State == nil {
			if c.State == "exited" || c.State == "stopped" {
				return clusterapi.PhaseReasonContainerFailed, "the engine reports no state for the exited container, so how its command ended is unknown", restartCount, scheduledNodes, nil
			}
			return "", "", restartCount, scheduledNodes, nil
		}
		if resp.Container.State.Error != "" {
			return podexecPhaseReason(resp.Container.State.Error), resp.Container.State.Error, restartCount, scheduledNodes, nil
		}
		// The states PollJobStatus does not report as what the job itself did.
		switch c.State {
		case "running", "created", "restarting", "removing", "exited", "stopped":
		case "paused":
			return "", "the container is paused on this node's engine, so the job's command does not run until somebody unpauses it", restartCount, scheduledNodes, nil
		case "dead":
			return clusterapi.PhaseReasonContainerFailed, "the engine reports the container dead: it could neither run it nor remove it", restartCount, scheduledNodes, nil
		default:
			return clusterapi.PhaseReasonContainerFailed, fmt.Sprintf("the engine reports the container in state %q, which this runtime does not know", c.State), restartCount, scheduledNodes, nil
		}
		if resp.Container.State.ExitCode != 0 {
			return clusterapi.PhaseReasonOfExit(resp.Container.State.ExitCode), fmt.Sprintf("container exited with code %d", resp.Container.State.ExitCode), restartCount, scheduledNodes, nil
		}
		return "", "", restartCount, scheduledNodes, nil
	}
	if failed, ok := e.pullFailures()[experimentID]; ok && failed.reason == "" {
		return "", failed.message, 0, 1, nil
	}
	if busy && op.starting {
		return "", "", 0, 1, nil
	}
	return "", "", 0, 0, nil
}

// pullFailureReason is the reason an image can never be pulled, read from the class the engine
// answered the pull with (the registry's own status, which podman and docker both pass on), and
// empty for a failure worth retrying. A registry that answers a missing or private image with
// anything else reaches here as an engine error like any outage and is retried. A malformed
// reference is refused with no class of its own, in the words podexecPhaseReason reads.
func pullFailureReason(err error) string {
	switch {
	case errdefs.IsNotFound(err), errdefs.IsUnauthorized(err), errdefs.IsPermissionDenied(err):
		return clusterapi.PhaseReasonImagePullFailed
	case errdefs.IsInvalidArgument(err):
		return clusterapi.PhaseReasonInvalidImage
	default:
		return podexecPhaseReason(err.Error())
	}
}

// podexecPhaseReason classifies a container engine's own error text into the control plane's
// generic clusterapi.PhaseReason* vocabulary — the control plane must never learn the engine's own
// error strings (important.md #7). Unrecognized text reports message only, no reason.
func podexecPhaseReason(engineError string) string {
	lower := strings.ToLower(engineError)
	switch {
	case containsAny(lower, "no such image", "pull access denied", "manifest unknown", "not found: manifest"):
		return clusterapi.PhaseReasonImagePullFailed
	case containsAny(lower, "invalid reference format"):
		return clusterapi.PhaseReasonInvalidImage
	default:
		return ""
	}
}

func containsAny(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
