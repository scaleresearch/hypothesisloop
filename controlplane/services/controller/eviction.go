package controller

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
	"github.com/scaleresearch/openresearch/controlplane/shared/obsmetrics"
)

// evict marks the experiment EVICTED, terminates the backend workload, and refunds unused accelerator
// hours — status transition, eviction reason, and every resource-dimension refund happen in one
// DB transaction (TransitionAndRefund), so a crash mid-eviction can never leave a durably
// EVICTED experiment with a partial refund.
func (c *Controller) evict(ctx context.Context, exp *domain.Experiment, reason domain.EvictionReason, now time.Time) error {
	updated, err := c.store.TransitionTerminal(ctx, exp.ID, exp.Status, domain.StatusEvicted, string(reason))
	if err != nil {
		return fmt.Errorf("evict: %w", err)
	}
	if !updated {
		// Job already left exp.Status (completed or cancelled concurrently) — skip settlement.
		return nil
	}
	obsmetrics.EvictedExperimentsTotal.WithLabelValues(string(reason)).Inc()
	// exp may have been ADMITTED/SUBMITTED (not yet RUNNING) when evicted — release any
	// pending-capacity reservation so it doesn't permanently blackhole that capacity slice.
	_ = c.store.DeletePendingReservation(ctx, exp.ID)

	// Status is already EVICTED above — the cluster-agent's next reconcile pass removes the
	// Job on its own. Settling final usage is a separate, independently-retryable step (see
	// settleAndMark) — its failure must never undo or block the transition above.
	exp.Status = domain.StatusEvicted
	exp.EvictionReason = string(reason)
	c.settleAndMark(ctx, exp)

	c.logger.Info("experiment evicted",
		zap.String("id", exp.ID),
		zap.String("reason", string(reason)),
	)

	if c.loop != nil {
		c.loop.Trigger()
	}
	return nil
}

// reconcileClosedExperiments evicts any jobs still active for closed platform experiments.
// Self-healing complement to Close(): if close succeeded in the DB but pod termination or
// refunds failed, the next reconcile tick finishes the cleanup automatically.
func (c *Controller) reconcileClosedExperiments(ctx context.Context) error {
	closedPEs, err := c.phase2Store.ListPlatformExperiments(ctx, "closed")
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
				updated, err := c.store.TransitionTerminal(ctx, exp.ID, exp.Status, domain.StatusRejected,
					string(domain.EvictionExperimentClosed))
				if err != nil {
					c.logger.Error("reconcileClosedExperiments: cancel pre-run job",
						zap.String("exp", exp.ID), zap.Error(err))
					continue
				}
				if !updated {
					continue
				}
				obsmetrics.EvictedExperimentsTotal.WithLabelValues(string(domain.EvictionExperimentClosed)).Inc()
				// SUBMITTED jobs may hold a pending reservation (QUEUED never does) — release
				// it unconditionally; a no-op if none exists.
				_ = c.store.DeletePendingReservation(ctx, exp.ID)
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
