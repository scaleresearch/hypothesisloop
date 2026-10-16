package cloudcompute

import (
	"context"
	"fmt"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/provider"
)

// FetchLogTail relays the end of the newest instance's output. A starting instance's command has
// written nothing yet, the output of one the provider cannot place need not be readable, and no
// instance is no output, which is an ordinary state between two attempts.
func (e *Executor) FetchLogTail(ctx context.Context, experimentID string, maxLines int) ([]string, error) {
	v, err := e.current(ctx)
	if err != nil {
		return nil, err
	}
	i, ok := v.newest(experimentID)
	if !ok || i.State == provider.StateStarting || i.State == provider.StateUnknown {
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
// with the cluster's stuck-pending timeout. Once the command runs the job is on its node, and it
// stays there while the provider does not know whether the command still runs.
func (e *Executor) PollPhaseDetail(ctx context.Context, experimentID string, attempt int) (reason, message string, restartCount int32, scheduledNodes int32, schedulingReason string, err error) {
	v, err := e.current(ctx)
	if err != nil {
		return "", "", 0, 0, "", err
	}
	i, ok := v.newest(experimentID)
	if !ok || (attempt != clusterapi.AttemptUnknown && i.attempt != attempt) {
		return "", "", 0, 0, "", nil
	}
	if i.State == provider.StateStarting {
		return clusterapi.PhaseReasonInstanceStarting, fmt.Sprintf("a machine is rented and has not started the job yet; it is replaced by another if it has not within %s", e.startup), 0, 0, "", nil
	}
	if i.State == provider.StateUnknown {
		return clusterapi.PhaseReasonInstanceUnknown, "the provider lists the job's machine and does not say whether the job runs on it: its host is unreachable or the provider is moving it", 0, 1, "", nil
	}
	if i.State != provider.StateExited {
		return "", "", 0, 1, "", nil
	}
	if how, code, _ := e.ending(ctx, v, i); how == endingFailed {
		return clusterapi.PhaseReasonOfExit(code), fmt.Sprintf("command exited with code %d", code), 0, 1, "", nil
	}
	return "", "", 0, 1, "", nil
}
