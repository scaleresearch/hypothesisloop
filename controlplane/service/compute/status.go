package compute

import (
	"context"
	"fmt"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/ranktail"
)

// FetchLogTail relays the end of the output of experimentID's newest attempt: of its one
// instance, or for a job of several ranks one section per rank (ranktail). A starting instance's
// command has written nothing yet, the output of one the provider cannot place need not be
// readable, and no instance is no output, which is an ordinary state between two attempts.
func (e *Executor) FetchLogTail(ctx context.Context, experimentID string, maxLines int) ([]string, error) {
	v, err := e.current(ctx)
	if err != nil {
		return nil, err
	}
	ranks := v.gang(experimentID)
	if len(ranks) == 0 {
		return nil, nil
	}
	tail := func(i instance) func(n int) ([]string, error) {
		return func(n int) ([]string, error) {
			if i.State == provider.StateStarting || i.State == provider.StateUnknown {
				return nil, nil
			}
			read := e.output(ctx, v, i)
			if read.err != nil {
				return nil, read.err
			}
			return read.lines[max(0, len(read.lines)-n):], nil
		}
	}
	if ranks[0].ranks == 1 {
		return tail(ranks[0])(maxLines)
	}
	sections := make([]ranktail.Section, 0, len(ranks))
	for _, i := range ranks {
		s := ranktail.Section{Rank: i.rank, Where: "instance " + i.Name, Read: tail(i)}
		if i.State == provider.StateExited {
			if how, code, _ := e.ending(ctx, v, i); how == endingFailed {
				line, _ := clusterapi.FindExitLine(e.output(ctx, v, i).lines)
				s.Exit, s.FailedAt = code, line.EndedAt
			}
		}
		sections = append(sections, s)
	}
	return ranktail.Tail(sections, maxLines)
}

// PollPhaseDetail explains the ranks of an attempt that have not started or ended badly, and
// says how many of them have landed.
//
// A starting instance has not landed: the provider may still be matching an offer or booting a
// machine that never comes up, which is the wait the control plane bounds with the cluster's
// stuck-pending timeout. Once the command runs the rank is on its node, and it stays there while
// the provider does not know whether the command still runs. A rank that failed explains the job
// first, then one the provider cannot place, then one that has not started.
func (e *Executor) PollPhaseDetail(ctx context.Context, experimentID string, attempt int) (reason, message string, restartCount int32, scheduledNodes int32, err error) {
	v, err := e.current(ctx)
	if err != nil {
		return "", "", 0, 0, err
	}
	ranks := v.gang(experimentID)
	if len(ranks) == 0 || (attempt != clusterapi.AttemptUnknown && ranks[0].attempt != attempt) {
		return "", "", 0, 0, nil
	}
	var starting, unknown, failed string
	for _, i := range ranks {
		switch i.State {
		case provider.StateStarting:
			if starting == "" {
				starting = fmt.Sprintf("a machine is rented and has not started the job yet; it is replaced by another if it has not within %s", e.startup)
			}
			continue
		case provider.StateUnknown:
			if unknown == "" {
				unknown = "the provider lists the job's machine and does not say whether the job runs on it: its host is unreachable or the provider is moving it"
			}
		case provider.StateExited:
			if how, code, _ := e.ending(ctx, v, i); how == endingFailed && failed == "" {
				reason, failed = clusterapi.PhaseReasonOfExit(code), fmt.Sprintf("command exited with code %d", code)
				if i.ranks > 1 {
					failed = fmt.Sprintf("rank %d: %s", i.rank, failed)
				}
			}
		}
		scheduledNodes++
	}
	switch {
	case failed != "":
		return reason, failed, 0, scheduledNodes, nil
	case unknown != "":
		return clusterapi.PhaseReasonInstanceUnknown, unknown, 0, scheduledNodes, nil
	case starting != "":
		return clusterapi.PhaseReasonInstanceStarting, starting, 0, scheduledNodes, nil
	}
	return "", "", 0, scheduledNodes, nil
}
