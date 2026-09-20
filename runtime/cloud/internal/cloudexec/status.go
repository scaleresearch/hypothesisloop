package cloudexec

import (
	"context"
	"fmt"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/workload"
	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/exitline"
)

// exitLineTail is how far back from the end of an exited instance's output the exit line is
// looked for. The wrapper writes it last; the margin is for output of the command's own children
// that reaches the vendor's log after it.
const exitLineTail = 50

// outcome is how an exited instance's command ended. reported is false when the output carries
// no exit line, which means the wrapper did not get to write one: the instance was killed from
// outside, ran out of memory, or the image has no hl.
type outcome struct {
	code     int
	reported bool
}

func (e *Executor) outcome(ctx context.Context, i instance) (outcome, error) {
	lines, err := e.provider.Logs(ctx, i.ID, exitLineTail)
	if err != nil {
		return outcome{}, fmt.Errorf("cloudexec: read exit line of instance %s: %w", i.Name, err)
	}
	code, reported := exitline.Find(lines)
	return outcome{code: code, reported: reported}, nil
}

// FetchLogTail relays the newest instance's output. No instance is no output, which is an
// ordinary state between two attempts.
func (e *Executor) FetchLogTail(ctx context.Context, experimentID string, maxLines int) ([]string, error) {
	instances, err := e.list(ctx)
	if err != nil {
		return nil, err
	}
	i, ok := newest(instances, experimentID)
	if !ok {
		return nil, nil
	}
	lines, err := e.provider.Logs(ctx, i.ID, maxLines)
	if err != nil {
		return nil, fmt.Errorf("cloudexec: logs of instance %s: %w", i.Name, err)
	}
	return lines, nil
}

// PollPhaseDetail explains an instance that ended badly and says whether the job has landed.
//
// A starting instance has not landed (scheduledNodes 0): the provider may still be matching an
// offer or booting a machine that never comes up, which is the wait the control plane bounds
// with the cluster's stuck-pending timeout. Once the command runs the job is on its node.
func (e *Executor) PollPhaseDetail(ctx context.Context, experimentID string, attempt int) (reason, message string, restartCount int32, scheduledNodes int32, schedulingReason string, err error) {
	instances, err := e.list(ctx)
	if err != nil {
		return "", "", 0, 0, "", err
	}
	i, ok := newest(instances, experimentID)
	if !ok || (attempt != workload.AttemptUnknown && i.attempt != attempt) {
		return "", "", 0, 0, "", nil
	}
	switch i.State {
	case provider.StateRunning:
		return "", "", 0, 1, "", nil
	case provider.StateExited:
		outcome, err := e.outcome(ctx, i)
		if err != nil {
			return "", "", 0, 1, "", err
		}
		switch {
		case !outcome.reported:
			return "", "instance ended without an exit line: the job's wrapper was killed or never ran", 0, 1, "", nil
		case outcome.code != 0:
			return domain.PhaseReasonContainerFailed, fmt.Sprintf("command exited with code %d", outcome.code), 0, 1, "", nil
		}
		return "", "", 0, 1, "", nil
	}
	return "", "", 0, 0, "", nil
}
