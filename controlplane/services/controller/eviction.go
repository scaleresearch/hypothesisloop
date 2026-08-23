package controller

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/obsmetrics"
)

// evict ends the experiment with the given reason and settles what it genuinely consumed. The
// store resolves what that means: an infrastructure fault with requeue budget left goes back to
// QUEUED for a free attempt instead of terminating, and one that exhausts that budget ends
// EVICTED and settles to a full refund — see db.Store.ResolveTermination and settlement.Settle.
// Nothing here decides that; this function only reports the outcome and settles either way.
func (c *Controller) evict(ctx context.Context, exp *domain.Experiment, reason domain.EvictionReason, now time.Time) error {
	outcome, err := c.store.ResolveTermination(ctx, exp.ID, exp.Status, domain.StatusEvicted, string(reason))
	if err != nil {
		return fmt.Errorf("evict: %w", err)
	}
	switch outcome {
	case domain.TerminationSkipped:
		// Job already left exp.Status (completed or cancelled concurrently) — skip settlement.
		return nil
	case domain.TerminationRequeued:
		// Bill what this attempt burned before it goes back in the queue, so a job cycling
		// through requeues is not invisible to running-cost and quota exhaustion meanwhile. Not
		// the refund: that lands only if the job ends in an infrastructure fault once its requeue
		// allowance runs out — see settlement.Settle. Deliberately not MarkQuotaSettled, since
		// the row is QUEUED again rather than terminal.
		exp.Status = domain.StatusQueued
		exp.EvictionReason = string(reason)
		if err := c.settler.Settle(ctx, exp); err != nil {
			c.logger.Warn("settle before infrastructure requeue", zap.String("id", exp.ID), zap.Error(err))
		}
		c.logger.Info("experiment requeued after an infrastructure fault",
			zap.String("id", exp.ID), zap.String("reason", string(reason)))
		return nil
	}
	obsmetrics.EvictedExperimentsTotal.WithLabelValues(string(reason)).Inc()
	// Settling final usage is a separate, independently-retryable step (see settleAndMark) —
	// its failure must never undo or block the transition above.
	exp.Status = domain.StatusEvicted
	exp.EvictionReason = string(reason)
	c.settleAndMark(ctx, exp)

	c.logger.Info("experiment evicted",
		zap.String("id", exp.ID),
		zap.String("reason", string(reason)),
	)

	return nil
}

// reconcileClosedExperiments evicts any jobs still active for closed platform experiments.
// Self-healing complement to Close(): if close succeeded in the DB but pod termination or
// refunds failed, the next reconcile tick finishes the cleanup automatically.
func (c *Controller) reconcileClosedExperiments(ctx context.Context) error {
	closedPEs, err := c.stagesStore.ListPlatformExperimentsByStatus(ctx, domain.PlatformExpClosed)
	if err != nil {
		return fmt.Errorf("reconcileClosedExperiments: list closed PEs: %w", err)
	}
	if len(closedPEs) == 0 {
		return nil
	}

	now := time.Now().UTC()
	for _, pe := range closedPEs {
		active, err := c.store.ListActiveByPlatformExperiment(ctx, pe.ID)
		if err != nil {
			c.logger.Error("reconcileClosedExperiments: list active jobs",
				zap.String("pe", pe.ID), zap.Error(err))
			continue
		}
		for _, exp := range active {
			if exp.Status == domain.StatusRunning || exp.Status == domain.StatusAdmitted {
				if err := c.evict(ctx, exp, domain.EvictionExperimentClosed, now); err != nil {
					c.logger.Error("reconcileClosedExperiments: evict running job",
						zap.String("exp", exp.ID), zap.Error(err))
				}
			} else {
				// QUEUED or SUBMITTED: cancel — never started, so Settle refunds it to 0.
				outcome, err := c.store.ResolveTermination(ctx, exp.ID, exp.Status, domain.StatusRejected,
					string(domain.EvictionExperimentClosed))
				if err != nil {
					c.logger.Error("reconcileClosedExperiments: cancel pre-run job",
						zap.String("exp", exp.ID), zap.Error(err))
					continue
				}
				if outcome != domain.TerminationWritten {
					continue
				}
				obsmetrics.EvictedExperimentsTotal.WithLabelValues(string(domain.EvictionExperimentClosed)).Inc()
				exp.Status = domain.StatusRejected
				exp.EvictionReason = string(domain.EvictionExperimentClosed)
				c.settleAndMark(ctx, exp)
				c.logger.Info("reconcileClosedExperiments: cancelled pre-run job",
					zap.String("pe", pe.ID), zap.String("exp", exp.ID))
			}
		}
	}
	return nil
}
