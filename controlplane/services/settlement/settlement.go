// Package settlement is the single place a terminal experiment's final observed usage gets
// written to the metrics DB — used by every termination path (natural completion, stuck-pending
// eviction, every controller eviction reason) so they all settle and retry the same way.
//
// The write happens after the already-committed status transition to COMPLETED/FAILED/EVICTED,
// so it can never share a transaction with it: Postgres and the metrics DB are separate stores.
// A crash or metrics-DB outage between the status commit and this write would otherwise leave
// the reservation stuck at its estimate forever. Settle is idempotent (recomputes purely from
// confirmed-alive time and writes an absolute value — see metricsdb.UsageTracker.SetObserved),
// so Reconciler can retry it any number of times, from any process, until it succeeds.
package settlement

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
)

// UsageWriter is the narrow slice of quota.PlatformExperimentsService (or any equivalent)
// Settle needs: an idempotent absolute-set write of one resource dimension's final cost.
type UsageWriter interface {
	SetObservedUsage(ctx context.Context, exp *domain.Experiment, amounts map[domain.ResourceType]float64) error
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

// New constructs a Settler. gapCap/step/maxLookback must match every other observed-usage query
// in this deployment, so every termination path agrees on what "how long did this run" means.
func New(usage UsageWriter, metricsDBURL string, gapCap, step, maxLookback time.Duration) *Settler {
	return &Settler{usage: usage, metricsDBURL: metricsDBURL, gapCap: gapCap, step: step, maxLookback: maxLookback}
}

// Settle writes exp's final observed cost for every resource dimension it was estimated for.
// Safe to call any number of times, from any process: it always recomputes from the metrics
// DB's confirmed-alive record rather than in-memory or caller-supplied state, and every write is
// an absolute set (never a delta).
//
// Special case: exp has no execution observation in metrics storage — it consumed nothing, so
// every reserved dimension settles to 0 regardless of why it was terminated (e.g. a queued job
// cancelled by a sibling exhausting budget; it never ran, so it owes nothing).
func (s *Settler) Settle(ctx context.Context, exp *domain.Experiment) error {
	if exp.PlatformExperimentID == "" {
		return nil
	}
	now := time.Now().UTC()
	_, observed, err := metricsdb.FirstObserved(ctx, s.metricsDBURL, exp.ID, now, s.maxLookback, s.step)
	if err != nil {
		return fmt.Errorf("settlement: first observed: %w", err)
	}
	neverStarted := !observed
	var hours, acceleratorCost float64
	if !neverStarted && exp.EstimatedDurationHours > 0 {
		var err error
		hours, err = metricsdb.ObservedElapsedHours(ctx, s.metricsDBURL, exp.ID, now, s.maxLookback, s.gapCap, s.step)
		if err != nil {
			return fmt.Errorf("settlement: observed elapsed hours: %w", err)
		}

		acceleratorCost, err = metricsdb.ObservedAcceleratorCost(ctx, s.metricsDBURL, exp.ID, exp.AcceleratorCount, now, s.maxLookback, s.gapCap, s.step)
		if err != nil {
			return fmt.Errorf("settlement: observed accelerator cost: %w", err)
		}
	}

	// CPU/RAM/storage have no independent "actual" measurement like accelerator does (which
	// comes from real alive-time samples) — the best signal is each dimension's estimated
	// per-hour rate (estimate / duration) times cumulative observed hours. This rate is
	// invariant across a preemption requeue's proportional rescale (see
	// experiments_store_lifecycle.RequeuePreempted), so it's correct whether or not the job was
	// preempted. The old approach (observed/currentEstimate) overcharged post-preemption jobs,
	// since currentEstimate only covers the shortened remaining work.
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
		{domain.ResourceAcceleratorHours, exp.EstimatedCostAccH, acceleratorCost},
		{domain.ResourceCPUCoreHours, exp.EstimatedCPUCoreHours, rateCost(exp.EstimatedCPUCoreHours)},
		{domain.ResourceRAMGBHours, exp.EstimatedRAMGBHours, rateCost(exp.EstimatedRAMGBHours)},
		{domain.ResourceStorageGBHours, exp.EstimatedStorageGBHours, rateCost(exp.EstimatedStorageGBHours)},
	}
	amounts := make(map[domain.ResourceType]float64, len(dims))
	for _, d := range dims {
		// Nothing was ever reserved on this dimension — no series to settle.
		if d.estimated <= 0 {
			continue
		}
		amounts[d.resourceType] = d.amount
	}
	if err := s.usage.SetObservedUsage(ctx, exp, amounts); err != nil {
		return fmt.Errorf("settlement: write observed usage: %w", err)
	}
	return nil
}

// Reconciler periodically retries settlement for terminal experiments Settle hasn't yet
// succeeded for — the durable-outbox retry loop for the crash/outage window described above.
// Safe to run alongside processes that settle inline (job_watcher, controller): a row already
// settled by one simply won't show up in the other's scan.
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
