// Package settlement is the single place a terminal experiment's final observed usage gets
// written to the metrics DB — used by every termination path (natural completion, stuck-pending
// eviction, and every controller eviction reason) so they all settle the same way and can all be
// retried the same way.
//
// The write happens after the (already-committed, durable) status transition to
// COMPLETED/FAILED/EVICTED, so it can never share a transaction with it: Postgres and the
// metrics DB are two separate stores. A crash or metrics-DB outage between the status commit
// and this write would otherwise leave the reservation stuck at its estimate forever. Settle is
// idempotent (it recomputes purely from confirmed-alive time in the metrics DB and writes an
// absolute value — see metricsdb.UsageTracker.SetObserved), so Reconciler can retry it any number
// of times, from any process, until it succeeds.
package settlement

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
	"github.com/scaleresearch/openresearch/controlplane/shared/metricsdb"
)

// UsageWriter is the narrow slice of quota.PlatformExperimentsService (or any equivalent)
// Settle needs: an idempotent absolute-set write of one resource dimension's final cost.
type UsageWriter interface {
	RefundQuota(ctx context.Context, agentID, platformExpID, experimentID string, resourceType domain.ResourceType, tier domain.CapacityTier, amount float64) error
}

// Store is the persistence interface Reconciler needs to find and mark settlement work.
type Store interface {
	ListUnsettledTerminalExperiments(ctx context.Context) ([]*domain.Experiment, error)
	MarkQuotaSettled(ctx context.Context, id string) error
}

// Settler computes and durably writes a terminal experiment's final observed usage across every
// resource dimension it was estimated for.
type Settler struct {
	usage        UsageWriter
	metricsDBURL string
	gapCap       time.Duration
	step         time.Duration
	maxLookback  time.Duration
}

// New constructs a Settler. gapCap/step/maxLookback must match whatever every other
// observed-usage query in this deployment uses (see metricsdb.ObservedElapsedHours), so every
// termination path agrees on what "how long did this run" means.
func New(usage UsageWriter, metricsDBURL string, gapCap, step, maxLookback time.Duration) *Settler {
	return &Settler{usage: usage, metricsDBURL: metricsDBURL, gapCap: gapCap, step: step, maxLookback: maxLookback}
}

// Settle writes exp's final observed cost for every resource dimension it was estimated for.
// Safe to call any number of times for the same experiment, from any process: it always
// recomputes from the metrics DB's own confirmed-alive record rather than from any in-memory or
// caller-supplied state, and every write is an absolute set (never a delta).
//
// Two special cases, matching every eviction path's original behavior:
//   - exp never reached RUNNING (StartedAt nil): it consumed nothing, so every dimension it
//     reserved settles to 0, regardless of why it was terminated — its reservation must still be
//     returned even if the *reason* string happens to be quota_exhaustion (e.g. a queued job
//     cancelled because a sibling job exhausted the budget; it never ran, so it owes nothing).
//   - exp reached RUNNING and was evicted for EvictionQuotaExhaustion specifically: its budget is
//     genuinely spent, so Settle is a deliberate no-op — the reservation stays charged at its
//     estimate rather than being trued up to the (possibly lower) observed cost.
func (s *Settler) Settle(ctx context.Context, exp *domain.Experiment) error {
	if exp.PlatformExperimentID == "" {
		return nil
	}
	neverStarted := exp.StartedAt == nil
	if !neverStarted && domain.EvictionReason(exp.EvictionReason) == domain.EvictionQuotaExhaustion {
		return nil
	}

	var hours, gpuCost float64
	if !neverStarted && exp.EstimatedDurationHours > 0 {
		now := time.Now().UTC()
		var err error
		hours, err = metricsdb.ObservedElapsedHours(ctx, s.metricsDBURL, exp.ID, now, s.maxLookback, s.gapCap, s.step)
		if err != nil {
			return fmt.Errorf("settlement: observed elapsed hours: %w", err)
		}

		gpuCost, err = metricsdb.ObservedGPUCost(ctx, s.metricsDBURL, exp.ID, exp.GPUCount, now, s.maxLookback, s.gapCap, s.step)
		if err != nil {
			return fmt.Errorf("settlement: observed GPU cost: %w", err)
		}
	}

	// CPU/RAM/storage have no independent "actual" measurement the way GPU does (ObservedGPUCost
	// comes from real alive-time samples) — the best available signal is each dimension's
	// estimated per-hour rate (estimate / duration) times real cumulative observed hours. This
	// rate is invariant across a preemption requeue's proportional rescale
	// (newEstimate/newDuration == oldEstimate/oldDuration, since both are scaled by the same
	// ratio — see experiments_store_lifecycle.RequeuePreempted), so multiplying by cumulative
	// observed hours here is correct whether or not the job was ever preempted. Using
	// observed/currentEstimate as a fraction instead (the old approach) overcharges a
	// post-preemption job, since currentEstimate only covers the remaining (shortened) work, not
	// the cumulative hours actually observed since the job first started.
	rateCost := func(estimated float64) float64 {
		if exp.EstimatedDurationHours <= 0 {
			return 0
		}
		return (estimated / exp.EstimatedDurationHours) * hours
	}

	dims := []struct {
		resourceType domain.ResourceType
		estimated    float64
		amount       float64
	}{
		{domain.ResourceGPUHours, exp.EstimatedCostT4H, gpuCost},
		{domain.ResourceCPUCoreHours, exp.EstimatedCPUCoreHours, rateCost(exp.EstimatedCPUCoreHours)},
		{domain.ResourceRAMGBHours, exp.EstimatedRAMGBHours, rateCost(exp.EstimatedRAMGBHours)},
		{domain.ResourceStorageGBHours, exp.EstimatedStorageGBHours, rateCost(exp.EstimatedStorageGBHours)},
	}
	for _, d := range dims {
		// Nothing was ever reserved on this dimension — no series to settle.
		if d.estimated <= 0 {
			continue
		}
		if err := s.usage.RefundQuota(ctx, exp.AgentID, exp.PlatformExperimentID, exp.ID, d.resourceType, exp.CapacityTier, d.amount); err != nil {
			return fmt.Errorf("settlement: refund %s: %w", d.resourceType, err)
		}
	}
	return nil
}

// Reconciler periodically retries settlement for terminal experiments Settle hasn't yet
// succeeded for — the durable-outbox retry loop for the crash/outage window described above.
// Safe to run in every process that also settles inline on its own termination path (job_watcher,
// controller): a row already settled by one simply won't show up in the other's scan.
type Reconciler struct {
	store    Store
	settler  *Settler
	logger   *zap.Logger
	interval time.Duration
}

// NewReconciler constructs a Reconciler that scans every interval.
func NewReconciler(store Store, settler *Settler, interval time.Duration, logger *zap.Logger) *Reconciler {
	return &Reconciler{store: store, settler: settler, interval: interval, logger: logger}
}

// Start runs the reconcile loop until ctx is cancelled.
func (r *Reconciler) Start(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.reconcileOnce(ctx)
		}
	}
}

func (r *Reconciler) reconcileOnce(ctx context.Context) {
	exps, err := r.store.ListUnsettledTerminalExperiments(ctx)
	if err != nil {
		r.logger.Error("settlement: list unsettled", zap.Error(err))
		return
	}
	for _, exp := range exps {
		if err := r.settler.Settle(ctx, exp); err != nil {
			r.logger.Warn("settlement: retry failed", zap.String("id", exp.ID), zap.Error(err))
			continue
		}
		if err := r.store.MarkQuotaSettled(ctx, exp.ID); err != nil {
			r.logger.Error("settlement: mark settled", zap.String("id", exp.ID), zap.Error(err))
		}
	}
}
