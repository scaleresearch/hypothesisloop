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
}

// New constructs a Settler. gapCap must match every other observed-usage query in this
// deployment, so every termination path agrees on what "how long did this run" means.
func New(usage UsageWriter, metricsDBURL string, gapCap time.Duration) *Settler {
	return &Settler{usage: usage, metricsDBURL: metricsDBURL, gapCap: gapCap}
}

// endedInInfrastructureFault reports whether exp's FINAL outcome was the environment's fault:
// EVICTED carrying an infrastructure-class reason, which is exactly the state
// db.Store.ResolveTermination writes when an infrastructure fault exhausts its requeue
// allowance. Both halves are required — see Settle for what keying on the reason alone cost.
//
// Deliberately derived here rather than kept true by clearing eviction_reason on every other
// terminal write: that would put correctness in the hands of each writer remembering to clean up
// (and any future one), and it would destroy a record other code depends on — a preempted job's
// retained `preempted_for_guaranteed` is what marks it as having already run, which is what
// forbids re-admitting it onto a different accelerator flavor.
func endedInInfrastructureFault(exp *domain.Experiment) bool {
	return exp.Status == domain.StatusEvicted &&
		domain.IsInfrastructureFault(domain.EvictionReason(exp.EvictionReason))
}

// Settle writes exp's final observed cost for every resource dimension it was estimated for.
// Safe to call any number of times, from any process: it always recomputes from the metrics
// DB's confirmed-alive record rather than in-memory or caller-supplied state, and every write is
// an absolute set (never a delta).
//
// A job with no observation at all consumed nothing, so every reserved dimension settles to 0
// regardless of why it was terminated (e.g. a queued job cancelled by a sibling exhausting
// budget; it never ran, so it owes nothing). A job that ENDED in an infrastructure fault settles
// to 0 for the same reason expressed differently — it ran, but not by its own choice.
//
// KNOWN LIMITATION, deliberately not fixed: the refund is whole-job, not per-attempt. A job that
// is infrastructure-requeued and then completes bills for every hour it ever ran, including the
// stints burned on broken hardware — ObservedElapsedHours is cumulative over the whole experiment
// window, and the attempt boundary is not recoverable from it. ObservedSpan.Stint looks like the
// missing piece but is not: it is derived purely from a gap in observations, so it cannot tell an
// infrastructure requeue from a preemption resume or a gang retry, and preemption is billed on
// the cumulative total by design (see the rate-invariance note below). Separating the stints
// would need each attempt tagged in the metrics store and a per-stint ledger to add them back up
// — real retained machinery, and a second record of a figure this store already holds. So the
// refund lands only when the job genuinely ends in an infrastructure fault: the ceiling case,
// where the environment gets the last word. A job the environment merely interrupted pays for
// the interruption.
func (s *Settler) Settle(ctx context.Context, exp *domain.Experiment) error {
	if exp.PlatformExperimentID == "" {
		return nil
	}
	now := time.Now().UTC()
	var hours float64
	// A job that ended in an infrastructure fault owes nothing. The environment failed it, so the
	// hours it burned were never the agent's choice to spend, and the refund is expressed here —
	// as zero observed hours flowing through the one absolute SetObservedUsage write below —
	// rather than as a credit issued by a second path. A separate refund write would be a second
	// authority over the same figure, free to disagree with this one and to double-apply on any of
	// the retries this function exists to be safe under; settling to zero cannot, because it is
	// recomputed from scratch and written absolutely every time.
	//
	// The status is half of the condition and not decoration. eviction_reason is a durable record
	// of what ended the LAST attempt and deliberately survives a requeue, so a row that was
	// infrastructure-requeued and later completed still carries `cluster_unreachable` while being
	// COMPLETED. Keying on the reason alone made that job — and any later workload-class failure
	// of it — settle to zero, so a single provoked infrastructure fault bought an agent an
	// unmetered run. Requiring EVICTED as well asks the question that actually matters, "did this
	// job END in an infrastructure fault", and reads it off the exact state ResolveTermination
	// writes when the requeue allowance runs out.
	if exp.EstimatedDurationHours > 0 && !endedInInfrastructureFault(exp) {
		var err error
		hours, err = metricsdb.ObservedElapsedHours(ctx, s.metricsDBURL, exp.ID, exp.CreatedAt, now, s.gapCap)
		if err != nil {
			return fmt.Errorf("settlement: observed elapsed hours: %w", err)
		}
	}
	// Every dimension, accelerator included, settles at its estimated per-hour rate (estimate /
	// duration) times cumulative observed hours. This rate is invariant across a preemption
	// requeue's proportional rescale (see experiments_store_lifecycle.RequeuePreempted), so it's
	// correct whether or not the job was preempted. The old approach (observed/currentEstimate)
	// overcharged post-preemption jobs, since currentEstimate only covers the shortened
	// remaining work.
	//
	// Accelerator used to be billed differently: metricsdb.ObservedAcceleratorCost queried a
	// separate "which type is this job actually running on right now" live marker per grid
	// point, to bill a mid-run reschedule onto a different type correctly segment-by-segment.
	// That marker depended on catching a job's RUNNING phase within a poll window — for jobs
	// that start and finish faster than one poll tick (routine for short benchmark jobs), it was
	// never written, so accelerator cost silently settled at 0 regardless of real runtime. The
	// flat per-job rate below can't bill a mid-run type change correctly, but a fixed
	// AcceleratorType per job's whole run is already the common case, and this needs no live
	// observation at all — it can't ever silently zero out.
	// Deliberately uncapped. A job that outran its estimate really did consume the hours, and
	// billing it for less would put a number in the metrics store that never happened — the one
	// thing that store is for. The bound on overrun is enforced upstream, not here: the
	// controller's quota-exhaustion check reads this same observed cost every reconcile tick and
	// evicts once the budget is spent, so observed usage can exceed an agent's budget by at most
	// one reconcile interval's worth of running jobs (plus a stage's max_job_hours cap, where the
	// platform experiment sets one).
	rateCost := func(estimated float64) float64 {
		return exp.RatedCost(estimated, hours)
	}

	dims := []struct {
		resourceType domain.ResourceType
		estimated    float64
		amount       float64
	}{
		{domain.ResourceAcceleratorHours, exp.EstimatedCostAccH, rateCost(exp.EstimatedCostAccH)},
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
