package podexec

import (
	"context"
	"fmt"
	"strings"

	"github.com/moby/moby/client"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// PollPhaseDetail inspects experimentID's container (if one exists yet) for an error and
// restart count. Read live on every call — no caching (important.md #4). A job whose image
// could never be pulled never gets a container to inspect at all; that case is left to the
// generic stuck-pending timeout rather than a runtime-side cache of the launch failure, which
// would be exactly the in-RAM state important.md #4 rules out. Returns empty reason/message and
// nil error when there is nothing notable to report.
func (e *Executor) PollPhaseDetail(ctx context.Context, experimentID string) (reason, message string, restartCount int32, scheduledNodes int32, schedulingReason string, err error) {
	containers, err := e.listManagedContainers(ctx)
	if err != nil {
		return "", "", 0, 0, "", err
	}
	for _, c := range containers {
		if c.Labels[LabelExperimentID] != experimentID {
			continue
		}
		resp, err := e.docker.ContainerInspect(ctx, c.ID, client.ContainerInspectOptions{})
		if err != nil {
			return "", "", 0, 0, "", fmt.Errorf("podexec: inspect container %s: %w", c.ID, err)
		}
		restartCount = int32(resp.Container.RestartCount)
		// Bare metal has no autoscaler and no gang concept -- one experiment, one container, on a
		// host that either exists or doesn't. scheduledNodes reports 1 once the container exists
		// (Docker created it, it has landed), matching how a k8s pod that has bound is counted.
		scheduledNodes = 1
		if resp.Container.State == nil {
			return "", "", restartCount, scheduledNodes, "", nil
		}
		if resp.Container.State.Error != "" {
			return podexecPhaseReason(resp.Container.State.Error), resp.Container.State.Error, restartCount, scheduledNodes, "", nil
		}
		if resp.Container.State.ExitCode != 0 {
			return "", fmt.Sprintf("container exited with code %d", resp.Container.State.ExitCode), restartCount, scheduledNodes, "", nil
		}
		return "", "", restartCount, scheduledNodes, "", nil
	}
	return "", "", 0, 0, "", nil
}

// podexecPhaseReason classifies a container engine's own error text into the control plane's
// generic domain.PhaseReason* vocabulary — the control plane must never learn the engine's own
// error strings (important.md #7). Unrecognized text reports message only, no reason.
func podexecPhaseReason(engineError string) string {
	lower := strings.ToLower(engineError)
	switch {
	case containsAny(lower, "no such image", "pull access denied", "manifest unknown", "not found: manifest"):
		return domain.PhaseReasonImagePullFailed
	case containsAny(lower, "invalid reference format"):
		return domain.PhaseReasonInvalidImage
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
