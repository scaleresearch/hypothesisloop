package cloudexec

import (
	"context"
	"fmt"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/workload"
	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
)

// FetchLogTail relays the end of the newest instance's output. A starting instance's command has
// written nothing yet, and no instance is no output, which is an ordinary state between two
// attempts.
func (e *Executor) FetchLogTail(ctx context.Context, experimentID string, maxLines int) ([]string, error) {
	v, err := e.current(ctx)
	if err != nil {
		return nil, err
	}
	i, ok := v.newest(experimentID)
	if !ok || i.State == provider.StateStarting {
		return nil, nil
	}
	read := e.output(ctx, v, i)
	if read.err != nil {
		return nil, read.err
	}
	return read.lines[max(0, len(read.lines)-maxLines):], nil
}

// PollPhaseDetail explains an instance that ended badly and says whether the job has landed.
//
// A starting instance has not landed (scheduledNodes 0): the provider may still be matching an
// offer or booting a machine that never comes up, which is the wait the control plane bounds
// with the cluster's stuck-pending timeout. Once the command runs the job is on its node.
func (e *Executor) PollPhaseDetail(ctx context.Context, experimentID string, attempt int) (reason, message string, restartCount int32, scheduledNodes int32, schedulingReason string, err error) {
	v, err := e.current(ctx)
	if err != nil {
		return "", "", 0, 0, "", err
	}
	i, ok := v.newest(experimentID)
	if !ok || (attempt != workload.AttemptUnknown && i.attempt != attempt) || i.State == provider.StateStarting {
		return "", "", 0, 0, "", nil
	}
	if i.State != provider.StateExited {
		return "", "", 0, 1, "", nil
	}
	switch how, code, readErr := e.ending(ctx, v, i); how {
	case endingUnread:
		return "", fmt.Sprintf("instance has exited and its output cannot be read yet: %v", readErr), 0, 1, "", nil
	case endingFailed:
		return domain.PhaseReasonContainerFailed, fmt.Sprintf("command exited with code %d", code), 0, 1, "", nil
	}
	return "", "", 0, 1, "", nil
}
