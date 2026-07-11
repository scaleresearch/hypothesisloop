package scheduler

import (
	"context"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
	"github.com/scaleresearch/openresearch/controlplane/shared/metricsdb"
	"github.com/scaleresearch/openresearch/controlplane/shared/obsmetrics"
)

// cpuFlavor is the pseudo-flavor key GetFlavorCapacity uses for live-reported CPU-core
// capacity (see queuebackend.Backend.GetFlavorCapacity). Kept in sync with that constant.
const cpuFlavor = "cpu"

// admissionUnit returns the capacity-map key and unit count exp consumes: GPU count against
// its GPU flavor for GPU jobs, or requested CPU cores against the cpuFlavor pseudo-flavor for
// CPU-only jobs (GPUCount == 0). One admission unit shared by the SUBMITTED-footprint
// subtraction, both admission passes, and preemption victim accounting, so all four always
// agree on what a job actually costs.
func admissionUnit(exp *domain.Experiment) (flavor string, need int64) {
	if exp.GPUCount > 0 {
		return exp.GPUType.FlavorName(), int64(exp.GPUCount)
	}
	return cpuFlavor, int64(math.Ceil(exp.RequestedCPUCores())) // ceil: round demand up, mirrors rounding available capacity down (queue_backend.go) — both directions err conservative
}

// LoopStore is the persistence interface required by the scheduler loop.
type LoopStore interface {
	ListQueuedExperiments(ctx context.Context) ([]*domain.Experiment, error)
	ListSubmittedExperiments(ctx context.Context) ([]*domain.Experiment, error)
	ListAdmittedExperiments(ctx context.Context) ([]*domain.Experiment, error)
	ListRunningExperiments(ctx context.Context) ([]*domain.Experiment, error)
	MarkQueued(ctx context.Context, id string) error
	MarkSubmitted(ctx context.Context, id string) error
	RequeuePreempted(ctx context.Context, id string, remainingHours float64) error
	UpdateExperimentStatus(ctx context.Context, id string, status domain.ExperimentStatus) error
	UpdateEvictionReason(ctx context.Context, id, reason string) error
	// UpdateNotAdmittedReason explains why a QUEUED job was skipped on its most recent tick
	// (capacity_unavailable | outranked | summary_gate); pass "" to clear it on admission.
	UpdateNotAdmittedReason(ctx context.Context, id, reason string) error
	// HasUnsummarizedCompleted returns true when the agent has a COMPLETED experiment
	// without a summary in the given platform experiment. Used to enforce the summary gate
	// during admission so that batch-submitted jobs cannot bypass it.
	HasUnsummarizedCompleted(ctx context.Context, agentID, platformExpID string) (bool, error)
}

// LoopQuotaStore handles quota bookkeeping for the loop. Preemption requeues the victim without
// refunding (see RequeuePreempted) — its estimate stays reserved until the job's eventual
// completion or eviction writes its real observed cost, same as every other termination path.
type LoopQuotaStore interface {
	GetAgentQuota(ctx context.Context, agentID, platformExpID string) (*domain.AgentQuota, error)
}

// LoopWorkloadClient is the backend-agnostic interface needed by the loop. No DeleteWorkload:
// preemption requeues the victim (status -> QUEUED) *before* calling WaitForJobDeletion —
// that status change is itself what tells the cluster-agent the Job should go away.
type LoopWorkloadClient interface {
	CreateWorkload(ctx context.Context, exp *domain.Experiment) error
	WaitForJobDeletion(ctx context.Context, exp *domain.Experiment, timeout time.Duration) error
	// GetFlavorCapacity returns available GPU slots per GPU flavor name
	// (e.g. "flavor-t4") for the guaranteed and burst tiers.
	GetFlavorCapacity(ctx context.Context) (guaranteed, burst map[string]int64, err error)
}

// Reprioritizer recomputes priority scores for all queued experiments.
// Implemented by *Service. Called after every admission pass so that new information
// (novelty changes, elapsed time, etc.) is always reflected in the queue ordering.
type Reprioritizer interface {
	RePrioritize(ctx context.Context) error
}

// Loop is the single-threaded scheduler loop. One goroutine, no concurrent ticks.
type Loop struct {
	store          LoopStore
	quota          LoopQuotaStore
	workload       LoopWorkloadClient
	reprioritizer  Reprioritizer // optional; called after each admission pass
	trigger        chan struct{}
	logger         *zap.Logger
	heartbeat      time.Duration
	preemptTimeout time.Duration
	// guaranteedFairnessWindow quantizes queued_at for sortGuaranteed's age-bucket fairness
	// tiebreak — see sortGuaranteed's doc comment.
	guaranteedFairnessWindow time.Duration

	// ticking enforces the single-threaded invariant tick() and preempt() capacity accounting
	// depend on for correctness (see competetors/SYNTHESIS_GAPS_AND_PLAN.md item #2). Unlike
	// cluster-agent's reconcile loop — which is safe to run concurrently because it's purely
	// declarative (diff desired vs. actual, converge) — tick() is a read-then-decide-then-write
	// capacity accounting operation: two concurrent ticks could each read the same "1 GPU free"
	// and both admit a job against it, a real double-booking, not an idempotent retry. This
	// turns that invariant into an enforced guarantee instead of a comment: it panics loudly on
	// reentrancy rather than silently double-admitting/double-preempting.
	ticking atomic.Bool

	// metricsDBURL, observedGapCap, observedStep let preempt() rank victims by real observed
	// runtime (metricsdb.ObservedElapsedHours) instead of wall-clock ElapsedHours() — a job stuck
	// in a reschedule/node-death gap for most of its wall-clock life isn't "the one that's made
	// the most progress" just because it was admitted a while ago.
	metricsDBURL   string
	observedGapCap time.Duration
	observedStep   time.Duration
}

// NewLoop constructs a Loop.
func NewLoop(store LoopStore, quota LoopQuotaStore, workloadClient LoopWorkloadClient, logger *zap.Logger) *Loop {
	return &Loop{
		store:          store,
		quota:          quota,
		workload:       workloadClient,
		trigger:        make(chan struct{}, 1),
		logger:         logger,
		heartbeat:                10 * time.Second,
		preemptTimeout:           30 * time.Second,
		guaranteedFairnessWindow: 60 * time.Second,
	}
}

func (l *Loop) WithHeartbeat(d time.Duration) *Loop {
	l.heartbeat = d
	return l
}

func (l *Loop) WithPreemptTimeout(d time.Duration) *Loop {
	l.preemptTimeout = d
	return l
}

// WithObservedTimeConfig wires the GreptimeDB URL and gap-cap/step preempt() uses to rank
// victims by real observed runtime — see metricsdb.ObservedElapsedHours. Pass the same values
// every other observed-time consumer in this deployment uses.
func (l *Loop) WithObservedTimeConfig(metricsDBURL string, gapCap, step time.Duration) *Loop {
	l.metricsDBURL = metricsDBURL
	l.observedGapCap = gapCap
	l.observedStep = step
	return l
}

func (l *Loop) WithGuaranteedFairnessWindow(d time.Duration) *Loop {
	l.guaranteedFairnessWindow = d
	return l
}

// WithReprioritizer attaches a Reprioritizer that runs after every admission pass.
func (l *Loop) WithReprioritizer(r Reprioritizer) *Loop {
	l.reprioritizer = r
	return l
}

// Trigger wakes the loop. Non-blocking: if already pending, does nothing.
func (l *Loop) Trigger() {
	select {
	case l.trigger <- struct{}{}:
	default:
	}
}

// Start runs the loop goroutine until ctx is cancelled.
func (l *Loop) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(l.heartbeat)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-l.trigger:
			case <-ticker.C:
			}
			if err := l.tick(ctx); err != nil {
				l.logger.Error("scheduler loop tick", zap.Error(err))
			}
		}
	}()
}

// tick runs one full admission pass. Single-threaded — no concurrent ticks.
func (l *Loop) tick(ctx context.Context) error {
	if !l.ticking.CompareAndSwap(false, true) {
		panic("scheduler: tick() re-entered concurrently — capacity accounting is not safe under concurrency, see Loop.ticking")
	}
	defer l.ticking.Store(false)

	start := time.Now()
	defer func() { obsmetrics.AdmissionTickDuration.Observe(time.Since(start).Seconds()) }()

	// 1. Get available physical capacity, keyed by flavor name.
	gAvail, bAvail, err := l.workload.GetFlavorCapacity(ctx)
	if err != nil {
		return err
	}

	// 2. Subtract the footprint of every job currently holding physical capacity: SUBMITTED
	// and ADMITTED (in-flight, not yet observed RUNNING) plus RUNNING. gAvail/bAvail are two
	// views of the *same* shared physical pool (see LoopWorkloadClient.GetFlavorCapacity's
	// doc comment), so a unit held by a job of either tier is unavailable in both views —
	// preemption, not a capacity split, is what enforces the tier boundary. Subtracting only
	// within the holder's own tier (as this used to) left the other tier's view at the full
	// nominal capacity forever, so once anything was RUNNING the guaranteed pass never saw a
	// shortfall and never triggered preempt().
	submitted, err := l.store.ListSubmittedExperiments(ctx)
	if err != nil {
		return err
	}
	admittedInFlight, err := l.store.ListAdmittedExperiments(ctx)
	if err != nil {
		return err
	}
	runningNow, err := l.store.ListRunningExperiments(ctx)
	if err != nil {
		return err
	}
	occupied := make([]*domain.Experiment, 0, len(submitted)+len(admittedInFlight)+len(runningNow))
	occupied = append(occupied, submitted...)
	occupied = append(occupied, admittedInFlight...)
	occupied = append(occupied, runningNow...)
	for _, exp := range occupied {
		flavor, need := admissionUnit(exp)
		gAvail[flavor] -= need
		if gAvail[flavor] < 0 {
			gAvail[flavor] = 0
		}
		bAvail[flavor] -= need
		if bAvail[flavor] < 0 {
			bAvail[flavor] = 0
		}
	}

	// 3. Get all QUEUED experiments.
	queued, err := l.store.ListQueuedExperiments(ctx)
	if err != nil {
		return err
	}

	// 3a. Enforce the summary gate: skip agents who have COMPLETED experiments without a
	// summary. The gate runs at POST /experiments submission time, but a batch of jobs
	// submitted before any run completes will all be in QUEUED already. We re-check here
	// so that completion of one job pauses the rest until summaries are written.
	summaryBlocked := map[string]bool{} // key: agentID+"/"+platformExpID
	filtered := queued[:0]
	for _, exp := range queued {
		if exp.PlatformExperimentID == "" {
			filtered = append(filtered, exp)
			continue
		}
		key := exp.AgentID + "/" + exp.PlatformExperimentID
		if blocked, seen := summaryBlocked[key]; seen {
			if !blocked {
				filtered = append(filtered, exp)
			}
			continue
		}
		blocked, err := l.store.HasUnsummarizedCompleted(ctx, exp.AgentID, exp.PlatformExperimentID)
		if err != nil {
			l.logger.Warn("summary gate check failed, allowing experiment",
				zap.String("exp", exp.ID), zap.Error(err))
			blocked = false
		}
		summaryBlocked[key] = blocked
		if !blocked {
			filtered = append(filtered, exp)
		} else {
			l.setNotAdmittedReason(ctx, exp.ID, domain.NotAdmittedSummaryGate)
		}
	}
	queued = filtered

	// 4. Guaranteed pass: FIFO by age bucket, quota-ratio tiebreak within a bucket, then
	// completion proximity DESC, then shortest job first.
	guaranteed := filterTier(queued, domain.CapacityGuaranteed)
	sortGuaranteed(guaranteed, l.fetchQuotaMap(ctx, guaranteed), l.guaranteedFairnessWindow)

	// Snapshot pre-tick availability so a skip can be classified: capacity_unavailable (no
	// capacity for this flavor existed even before this tick admitted anything) vs outranked
	// (capacity existed, but other guaranteed jobs earlier in this tick's sort order already
	// claimed it) — see #15 in competetors/SYNTHESIS_GAPS_AND_PLAN.md.
	gAvailInitial := make(map[string]int64, len(gAvail))
	for k, v := range gAvail {
		gAvailInitial[k] = v
	}

	for _, exp := range guaranteed {
		flavor, need := admissionUnit(exp)
		if need > gAvail[flavor] {
			// Doesn't fit — try to preempt burst jobs of the same flavor to make room.
			running, err := l.store.ListRunningExperiments(ctx)
			if err != nil {
				return err
			}
			burstRunning := filterTierAndFlavor(running, domain.CapacityBurst, flavor)
			freed, err := l.preempt(ctx, need-gAvail[flavor], burstRunning)
			if err != nil {
				l.logger.Warn("preemption failed", zap.String("exp", exp.ID), zap.Error(err))
				obsmetrics.AdmissionTickResultsTotal.WithLabelValues("guaranteed", "skipped").Inc()
				l.setNotAdmittedReason(ctx, exp.ID, notAdmittedReasonFor(gAvail[flavor], gAvailInitial[flavor]))
				continue
			}
			if freed > 0 {
				obsmetrics.AdmissionTickResultsTotal.WithLabelValues("guaranteed", "preempted").Add(float64(freed))
			}
			gAvail[flavor] += freed
			if need > gAvail[flavor] {
				obsmetrics.AdmissionTickResultsTotal.WithLabelValues("guaranteed", "skipped").Inc()
				l.setNotAdmittedReason(ctx, exp.ID, notAdmittedReasonFor(gAvail[flavor], gAvailInitial[flavor]))
				continue // not enough even after preemption
			}
		}
		if err := l.submitJob(ctx, exp); err != nil {
			l.logger.Error("submit guaranteed job", zap.String("exp", exp.ID), zap.Error(err))
			obsmetrics.AdmissionTickResultsTotal.WithLabelValues("guaranteed", "skipped").Inc()
			continue
		}
		gAvail[flavor] -= need
		// bAvail is the same shared physical pool's other view (see the capacity-accounting
		// comment on step 2 above) — without this, a job the burst pass admits later in this
		// same tick can still see the unit this guaranteed job just claimed as free and
		// double-book it, since bAvail was only synced against *pre-tick* occupancy.
		bAvail[flavor] -= need
		if bAvail[flavor] < 0 {
			bAvail[flavor] = 0
		}
		obsmetrics.AdmissionTickResultsTotal.WithLabelValues("guaranteed", "admitted").Inc()
		l.setNotAdmittedReason(ctx, exp.ID, "")
	}

	// 5. Burst pass: fairness-weighted (least quota used first), then completion proximity, shortest job first.
	burst := filterTier(queued, domain.CapacityBurst)
	if len(burst) == 0 {
		return nil
	}

	// Check if any burst capacity exists at all.
	var totalBAvail int64
	for _, v := range bAvail {
		totalBAvail += v
	}
	if totalBAvail <= 0 {
		for _, exp := range burst {
			obsmetrics.AdmissionTickResultsTotal.WithLabelValues("burst", "skipped").Inc()
			l.setNotAdmittedReason(ctx, exp.ID, domain.NotAdmittedCapacityUnavailable)
		}
		return nil
	}

	// Fetch quota usage for fairness ordering.
	quotaMap := l.fetchQuotaMap(ctx, burst)
	sortBurst(burst, quotaMap)

	bAvailInitial := make(map[string]int64, len(bAvail))
	for k, v := range bAvail {
		bAvailInitial[k] = v
	}

	for _, exp := range burst {
		flavor, need := admissionUnit(exp)
		if need > bAvail[flavor] {
			obsmetrics.AdmissionTickResultsTotal.WithLabelValues("burst", "skipped").Inc()
			l.setNotAdmittedReason(ctx, exp.ID, notAdmittedReasonFor(bAvail[flavor], bAvailInitial[flavor]))
			continue
		}
		if err := l.submitJob(ctx, exp); err != nil {
			l.logger.Error("submit burst job", zap.String("exp", exp.ID), zap.Error(err))
			obsmetrics.AdmissionTickResultsTotal.WithLabelValues("burst", "skipped").Inc()
			continue
		}
		bAvail[flavor] -= need
		obsmetrics.AdmissionTickResultsTotal.WithLabelValues("burst", "admitted").Inc()
		l.setNotAdmittedReason(ctx, exp.ID, "")
	}

	// Reprioritize all queued jobs after each admission pass so the queue order
	// stays fresh (novelty shifts as jobs start/finish, age increases, etc.).
	// This runs synchronously in the same goroutine — single-threaded, no races.
	if l.reprioritizer != nil {
		if err := l.reprioritizer.RePrioritize(ctx); err != nil {
			l.logger.Warn("reprioritize after tick", zap.Error(err))
		}
	}

	return nil
}

// preempt selects and evicts burst victims until neededGPUs are freed.
// Returns the number of GPUs actually freed.
func (l *Loop) preempt(ctx context.Context, neededGPUs int64, burstRunning []*domain.Experiment) (int64, error) {
	if len(burstRunning) == 0 {
		return 0, nil
	}

	// Rank by real observed runtime, not wall-clock ElapsedHours(): a job that spent most of its
	// wall-clock life in a reschedule/node-death gap hasn't actually made more progress than one
	// admitted more recently, and shouldn't be spared preemption on that basis. Computed once up
	// front (not in the sort comparator) since it's a GreptimeDB query per job. A query error is
	// logged and treated as 0 observed hours — the conservative choice for a "prefer to evict the
	// job that's made the least progress" heuristic, not a wall-clock fallback for accounting.
	elapsed := make(map[string]float64, len(burstRunning))
	for _, exp := range burstRunning {
		hours, err := metricsdb.ObservedElapsedHours(ctx, l.metricsDBURL, exp.ID, time.Now().UTC(), observedMaxLookback, l.observedGapCap, l.observedStep)
		if err != nil {
			l.logger.Warn("preempt: observed elapsed hours", zap.String("id", exp.ID), zap.Error(err))
			hours = 0
		}
		elapsed[exp.ID] = hours
	}

	// Sort victims: least elapsed time first (minimize wasted work), largest footprint
	// (GPU count, or CPU cores for CPU-only jobs) tiebreak so we free the needed capacity by
	// evicting the fewest possible jobs.
	sort.Slice(burstRunning, func(i, j int) bool {
		ei, ej := elapsed[burstRunning[i].ID], elapsed[burstRunning[j].ID]
		if ei != ej {
			return ei < ej
		}
		_, ni := admissionUnit(burstRunning[i])
		_, nj := admissionUnit(burstRunning[j])
		return ni > nj
	})

	// Select victims to free enough capacity.
	var selected []*domain.Experiment
	var freed int64
	for _, victim := range burstRunning {
		if freed >= neededGPUs {
			break
		}
		_, need := admissionUnit(victim)
		selected = append(selected, victim)
		freed += need
	}

	if len(selected) == 0 {
		return 0, nil
	}

	// Fill-back pass: footprints are heterogeneous, so the loop above can overshoot (e.g.
	// victim N-1 alone already covers neededGPUs but victim N still got selected). Walk
	// backward and reprieve any victim whose removal from the selected set still leaves enough
	// freed capacity — minimizes total evictions instead of evicting whatever fit first.
	for i := len(selected) - 1; i >= 0; i-- {
		_, vn := admissionUnit(selected[i])
		if freed-vn >= neededGPUs {
			freed -= vn
			selected = append(selected[:i], selected[i+1:]...)
		}
	}

	for _, victim := range selected {
		l.logger.Info("preempting burst job",
			zap.String("victim", victim.ID),
			zap.String("for_tier", "guaranteed"),
			zap.Float64("observed_elapsed_hours", elapsed[victim.ID]),
		)
	}

	// Requeue every victim FIRST (status RUNNING -> QUEUED), sequentially: this is the actual
	// "delete" instruction — it moves each victim out of the desired-running set a
	// cluster-agent reconciles against, before we ever wait for its Job to disappear. Doing
	// this before waiting is required, not just a style choice: if we waited first, the
	// cluster-agent would still see the (still-RUNNING) experiment as desired and would never
	// remove the Job, so WaitForJobDeletion would time out on every preemption.
	//
	// Do not refund quota on preemption: the job is returning to QUEUED and will run again,
	// so its remaining estimated cost must stay debited. The completion handler issues the
	// unused-budget refund when the job eventually finishes.
	var actualFreed int64
	for _, victim := range selected {
		remaining := victim.RemainingEstimatedHours()
		if err := l.store.RequeuePreempted(ctx, victim.ID, remaining); err != nil {
			l.logger.Error("requeue preempted job", zap.String("id", victim.ID), zap.Error(err))
			continue
		}
		_, need := admissionUnit(victim)
		actualFreed += need
	}

	// Now wait for each victim's Job to actually disappear, in parallel — avoids serialising
	// WaitForJobDeletion across hundreds of jobs. This is a best-effort bound on how long we
	// let the tick believe the freed capacity is truly free of real cluster resources before
	// moving on; a timeout here is logged, not fatal — the job will still be reconciled away
	// by the cluster-agent shortly after, just outside this tick's window.
	var wg sync.WaitGroup
	for _, victim := range selected {
		wg.Add(1)
		go func(v *domain.Experiment) {
			defer wg.Done()
			if err := l.workload.WaitForJobDeletion(ctx, v, l.preemptTimeout); err != nil {
				l.logger.Warn("wait for job deletion", zap.String("id", v.ID), zap.Error(err))
			}
		}(victim)
	}
	wg.Wait()

	return actualFreed, nil
}

// submitJob marks the experiment SUBMITTED in the DB first, then creates the backend workload.
// Order matters: marking first ensures the in-flight footprint is always visible to the next
// tick even if the backend write is slow. On backend failure we roll back to QUEUED so the job
// re-enters the queue rather than leaking in an untracked SUBMITTED state.
func (l *Loop) submitJob(ctx context.Context, exp *domain.Experiment) error {
	if err := l.store.MarkSubmitted(ctx, exp.ID); err != nil {
		return err
	}
	if err := l.workload.CreateWorkload(ctx, exp); err != nil {
		if rbErr := l.store.MarkQueued(ctx, exp.ID); rbErr != nil {
			l.logger.Error("rollback to QUEUED failed after workload creation error",
				zap.String("exp", exp.ID), zap.Error(rbErr))
		}
		return err
	}
	return nil
}

// fetchQuotaMap builds a map of agentID → used T4H fraction for burst ordering.
func (l *Loop) fetchQuotaMap(ctx context.Context, exps []*domain.Experiment) map[string]float64 {
	seen := map[string]bool{}
	result := map[string]float64{}
	for _, exp := range exps {
		key := exp.AgentID + "/" + exp.PlatformExperimentID
		if seen[key] || exp.PlatformExperimentID == "" {
			continue
		}
		seen[key] = true
		aq, err := l.quota.GetAgentQuota(ctx, exp.AgentID, exp.PlatformExperimentID)
		if err != nil || aq == nil {
			continue
		}
		if aq.GuaranteedT4Hours > 0 {
			result[exp.AgentID] = aq.UsedGuaranteedT4H / aq.GuaranteedT4Hours
		}
	}
	return result
}

// notAdmittedReasonFor classifies a skipped job: if current availability for its flavor still
// equals what it was before this tick touched it, nothing was ever available (capacity never
// existed this tick, independent of competition); if it's lower, other jobs admitted earlier in
// this same tick's sort order consumed it (outranked).
func notAdmittedReasonFor(current, initial int64) string {
	if current < initial {
		return domain.NotAdmittedOutranked
	}
	return domain.NotAdmittedCapacityUnavailable
}

// setNotAdmittedReason is a best-effort write — a failure here never blocks scheduling, it only
// degrades the "why hasn't this been admitted" debugging signal for one tick.
func (l *Loop) setNotAdmittedReason(ctx context.Context, expID, reason string) {
	if err := l.store.UpdateNotAdmittedReason(ctx, expID, reason); err != nil {
		l.logger.Warn("set not_admitted_reason", zap.String("exp", expID), zap.Error(err))
	}
}

// filterTier returns experiments of the given capacity tier.
func filterTier(exps []*domain.Experiment, tier domain.CapacityTier) []*domain.Experiment {
	var out []*domain.Experiment
	for _, e := range exps {
		if e.CapacityTier == tier {
			out = append(out, e)
		}
	}
	return out
}

// filterTierAndFlavor returns experiments of the given tier whose admission-unit flavor
// (GPU flavor, or the cpuFlavor pseudo-flavor for CPU-only jobs) matches flavorName.
func filterTierAndFlavor(exps []*domain.Experiment, tier domain.CapacityTier, flavorName string) []*domain.Experiment {
	var out []*domain.Experiment
	for _, e := range exps {
		if f, _ := admissionUnit(e); e.CapacityTier == tier && f == flavorName {
			out = append(out, e)
		}
	}
	return out
}

// sortGuaranteed sorts guaranteed-tier experiments:
// 1. age bucket ASC, quantized to fairnessWindow (oldest bucket first) — jobs within the same
//    bucket are treated as "arrived around the same time" rather than strictly ordered by exact
//    queued_at, so...
// 2. ...quotaMap ratio ASC (least-used-guaranteed-quota agent first) breaks ties within a bucket.
//    Without this, pure exact-timestamp FIFO lets an agent with a steady submission stream get
//    its jobs consistently admitted ahead of other agents' equally-entitled jobs purely because
//    it always has *a* job ready to submit right after the last one clears — this bounds that
//    latency-fairness gap the same way Kueue's DRS bounds time-to-admission within a tier,
//    without abandoning FIFO altogether (a job's age bucket still dominates once the gap between
//    two jobs exceeds fairnessWindow, so nothing waits indefinitely).
// 3. CompletionFraction DESC (finish interrupted work first)
// 4. EstimatedDurationHours ASC (shortest job first for faster feedback)
func sortGuaranteed(exps []*domain.Experiment, quotaMap map[string]float64, fairnessWindow time.Duration) {
	sort.SliceStable(exps, func(i, j int) bool {
		ei, ej := exps[i], exps[j]

		// Primary: age bucket (oldest queued_at, quantized to fairnessWindow, first).
		if ei.QueuedAt != nil && ej.QueuedAt != nil && fairnessWindow > 0 {
			bi := ei.QueuedAt.Truncate(fairnessWindow)
			bj := ej.QueuedAt.Truncate(fairnessWindow)
			if !bi.Equal(bj) {
				return bi.Before(bj)
			}
			// Same bucket: least-used-guaranteed-quota agent goes first.
			ri, rj := quotaMap[ei.AgentID], quotaMap[ej.AgentID]
			if ri != rj {
				return ri < rj
			}
		} else if ei.QueuedAt != nil && ej.QueuedAt != nil && !ei.QueuedAt.Equal(*ej.QueuedAt) {
			return ei.QueuedAt.Before(*ej.QueuedAt)
		}

		// Tiebreak 1: completion proximity DESC.
		cf := ei.CompletionFraction() - ej.CompletionFraction()
		if cf > 0.01 {
			return true
		}
		if cf < -0.01 {
			return false
		}

		// Tiebreak 2: fewest GPU-hours first (gpu_count × duration — prefers short OR small-GPU jobs).
		return ei.GPUHours() < ej.GPUHours()
	})
}

// sortBurst sorts burst-tier experiments:
// 1. quota utilisation ratio ASC (least used guaranteed quota goes first)
// 2. CompletionFraction DESC
// 3. EstimatedDurationHours ASC
// 4. queued_at ASC (final tiebreak)
func sortBurst(exps []*domain.Experiment, quotaMap map[string]float64) {
	sort.SliceStable(exps, func(i, j int) bool {
		ei, ej := exps[i], exps[j]

		ri := quotaMap[ei.AgentID]
		rj := quotaMap[ej.AgentID]
		if ri != rj {
			return ri < rj
		}

		cf := ei.CompletionFraction() - ej.CompletionFraction()
		if cf > 0.01 {
			return true
		}
		if cf < -0.01 {
			return false
		}

		// Prefer fewest GPU-hours (gpu_count × duration) — same idea as guaranteed sort.
		if ei.GPUHours() != ej.GPUHours() {
			return ei.GPUHours() < ej.GPUHours()
		}

		if ei.QueuedAt != nil && ej.QueuedAt != nil {
			return ei.QueuedAt.Before(*ej.QueuedAt)
		}
		return false
	})
}
