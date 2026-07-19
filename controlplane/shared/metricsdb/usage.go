package metricsdb

import (
	"context"
	"fmt"
	"sync"

	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
)

// usedHoursMetric is the single gauge every agent-quota "used" bucket is stored under,
// distinguished by labels — the metrics DB is the sole store for consumption; Postgres only
// ever holds the guaranteed/burst allocation (the operator-set capacity setting), never a copy
// of how much of it has been used.
const usedHoursMetric = "agent_quota_used_hours"

// A job's used-hours sample carries a kind label separating an admission-time estimate from a
// settled, observed actual. This is what lets phase-2/exhaustion read settled consumption + live
// running usage only, never a queued job's reservation (which would prematurely trip the boundary
// and cancel work). A single job owns both series over its lifetime: kindReserved is written at
// admission and zeroed at settlement; kindObserved is written (once, absolutely) at settlement.
//
//   - availability (CheckAndDebit) = allocation − Σ(reserved active) − Σ(observed settled): both
//     kinds count, so it sums the whole metric with no kind filter.
//   - phase-2 / exhaustion = Σ(observed settled) + live actual of running attempts: reads only
//     kindObserved, never kindReserved.
const (
	kindReserved = "reserved"
	kindObserved = "observed"
)

// UsageTracker is the sole read/write path for agent quota consumption (used_guaranteed_*/
// used_burst_* in the old Postgres schema).
//
// Every sample is tagged with experiment_id, so each job owns its own series — an "agent's used
// hours" bucket is never itself stored, only ever computed by summing that agent's per-job series
// at read time (sumUsed, TotalObservedT4H). This is what makes job completion/eviction accounting
// idempotent: writing a job's final cost (SetObserved) is an absolute set against that job's own
// series, so replaying the same completion event twice (e.g. after a crash) writes the same value
// instead of double-refunding or double-debiting — there is no shared counter for two writes to
// collide on.
//
// Reservation at admission (CheckAndDebit) is the one place that still needs to serialize
// concurrent writers: two jobs admitting at once must not both read the same "current total"
// and jointly overspend. lockFor only ever serializes within this one process, which is not
// enough now that control-service can run as >1 replica (see shared/leaderelection) and the HTTP
// admission path is not leader-gated — callers MUST additionally hold
// db.PlatformExperimentsStore.WithAdmissionLock (a Postgres advisory lock, cluster-wide) around
// CheckAndDebit; see quota.PlatformExperimentsService.CheckAndDebitQuota
type UsageTracker struct {
	dbURL string
	locks sync.Map // key: agentID+"/"+platformExpID -> *sync.Mutex
}

// NewUsageTracker constructs a tracker backed by the GreptimeDB instance at dbURL.
func NewUsageTracker(dbURL string) *UsageTracker {
	return &UsageTracker{dbURL: dbURL}
}

// URL returns the GreptimeDB base URL this tracker is backed by, for callers (e.g. db.Store)
// that need to make their own PopulateUsage/PopulateUsageOne calls against the same instance.
func (t *UsageTracker) URL() string { return t.dbURL }

func (t *UsageTracker) lockFor(agentID, platformExpID string) *sync.Mutex {
	key := agentID + "/" + platformExpID
	l, _ := t.locks.LoadOrStore(key, &sync.Mutex{})
	return l.(*sync.Mutex)
}

func labelsFor(agentID, platformExpID, experimentID string, resourceType domain.ResourceType, tier domain.CapacityTier, kind string) map[string]string {
	return map[string]string{
		"agent_id":               agentID,
		"platform_experiment_id": platformExpID,
		"experiment_id":          experimentID,
		"resource_type":          string(resourceType),
		"tier":                   string(tier),
		"kind":                   kind,
	}
}

// sumUsed returns the sum of every job's own used-hours sample for one (agent, pe, resource,
// tier) bucket — the live-queried equivalent of what used to be a single stored counter. 0 if no
// job has ever reserved anything in this bucket.
func (t *UsageTracker) sumUsed(ctx context.Context, agentID, platformExpID string, resourceType domain.ResourceType, tier domain.CapacityTier) (float64, error) {
	promQL := fmt.Sprintf(`sum(%s{agent_id=%q, platform_experiment_id=%q, resource_type=%q, tier=%q})`,
		usedHoursMetric, agentID, platformExpID, string(resourceType), string(tier))
	samples, err := QueryVector(ctx, t.dbURL, promQL)
	if err != nil {
		return 0, err
	}
	if len(samples) == 0 {
		return 0, nil
	}
	return samples[0].Value, nil
}

// jobUsed returns a single job's own reserved-kind sample, or 0 if it has never reserved anything
// in this bucket. Reads the reservation series specifically (Debit's surcharge adds to the
// outstanding reservation, not the settled observed value).
func (t *UsageTracker) jobUsed(ctx context.Context, agentID, platformExpID, experimentID string, resourceType domain.ResourceType, tier domain.CapacityTier) (float64, error) {
	promQL := fmt.Sprintf(`%s{agent_id=%q, platform_experiment_id=%q, experiment_id=%q, resource_type=%q, tier=%q, kind=%q}`,
		usedHoursMetric, agentID, platformExpID, experimentID, string(resourceType), string(tier), kindReserved)
	samples, err := QueryVector(ctx, t.dbURL, promQL)
	if err != nil {
		return 0, err
	}
	if len(samples) == 0 {
		return 0, nil
	}
	return samples[0].Value, nil
}

// CheckAndDebit atomically (within this process) verifies the agent's aggregate used+amount
// doesn't exceed limit and, if so, reserves amount against experimentID's own series. limit is
// the bucket's allocation (guaranteed_t4_hours etc), read from Postgres by the caller — the only
// cross-store read this package needs.
//
// This is a reservation, not an observation: at admission time the job hasn't run yet, so there
// is nothing to observe — amount is the estimate, and it stands until SetObserved overwrites it
// with the job's real cost on completion or eviction.
func (t *UsageTracker) CheckAndDebit(ctx context.Context, agentID, platformExpID, experimentID string, resourceType domain.ResourceType, tier domain.CapacityTier, amount, limit float64) error {
	mu := t.lockFor(agentID, platformExpID)
	mu.Lock()
	defer mu.Unlock()

	used, err := t.sumUsed(ctx, agentID, platformExpID, resourceType, tier)
	if err != nil {
		return fmt.Errorf("metricsdb.CheckAndDebit: %w", err)
	}
	if remaining := limit - used; remaining < amount {
		return fmt.Errorf("insufficient_%s_quota: need %.2f %s, have %.2f remaining", string(tier), amount, resourceType, remaining)
	}
	return WriteGauge(ctx, t.dbURL, usedHoursMetric, labelsFor(agentID, platformExpID, experimentID, resourceType, tier, kindReserved), amount)
}

// Debit adds amount to experimentID's own reservation, no aggregate balance check — used for
// system-level adjustments (e.g. flavor-substitution surcharge) that must apply even if they
// push the agent's total past its allocation.
func (t *UsageTracker) Debit(ctx context.Context, agentID, platformExpID, experimentID string, resourceType domain.ResourceType, tier domain.CapacityTier, amount float64) error {
	mu := t.lockFor(agentID, platformExpID+"/"+experimentID)
	mu.Lock()
	defer mu.Unlock()

	used, err := t.jobUsed(ctx, agentID, platformExpID, experimentID, resourceType, tier)
	if err != nil {
		return fmt.Errorf("metricsdb.Debit: %w", err)
	}
	return WriteGauge(ctx, t.dbURL, usedHoursMetric, labelsFor(agentID, platformExpID, experimentID, resourceType, tier, kindReserved), used+amount)
}

// SetReservation overwrites experimentID's own not-yet-final reservation with a new absolute
// amount — used when a job's outstanding estimate changes mid-lifecycle (currently: preemption
// requeue, which shortens the remaining estimate) and the debited reservation must be corrected
// to match, rather than left at the stale original-admission estimate. Unlike SetObserved (the
// terminal, "this job is done, here's what it really cost" write), this is written while the job
// is still active/queued and expected to be superseded by further corrections or the eventual
// SetObserved call — kept as a separate, narrowly-named method so neither caller has to reason
// about the other's semantics.
func (t *UsageTracker) SetReservation(ctx context.Context, agentID, platformExpID, experimentID string, resourceType domain.ResourceType, tier domain.CapacityTier, amount float64) error {
	return WriteGauge(ctx, t.dbURL, usedHoursMetric, labelsFor(agentID, platformExpID, experimentID, resourceType, tier, kindReserved), amount)
}

// SetObserved records experimentID's real, observed cost — computed by the caller from
// confirmed-alive time, never from a wall-clock guess (see Controller.ObservedElapsedHours) — as
// the terminal settlement write. It writes the observed-kind series to observedAmount and zeroes
// the reserved-kind series in one settling step: the job is done, so its outstanding estimate no
// longer stands. Both are absolute sets against series only this job ever writes to, so replaying
// this (a retried call, a crash-recovery replay) is idempotent and can never double-count.
//
// The split matters for phase-2/exhaustion (TotalObservedT4H), which reads only the observed
// series: a still-queued or running job's reservation never counts toward the boundary, so a large
// queued job can no longer prematurely trip phase 2. Availability (sumUsed) sums both kinds, so a
// running job's reservation still holds its allocation until it settles.
func (t *UsageTracker) SetObserved(ctx context.Context, agentID, platformExpID, experimentID string, resourceType domain.ResourceType, tier domain.CapacityTier, observedAmount float64) error {
	if observedAmount < 0 {
		observedAmount = 0
	}
	if err := WriteGauge(ctx, t.dbURL, usedHoursMetric, labelsFor(agentID, platformExpID, experimentID, resourceType, tier, kindObserved), observedAmount); err != nil {
		return err
	}
	return WriteGauge(ctx, t.dbURL, usedHoursMetric, labelsFor(agentID, platformExpID, experimentID, resourceType, tier, kindReserved), 0)
}

// PopulateUsage fills every quotas[i].Used* field from the metrics DB with a single query for
// the whole platform experiment (summed across every job that has ever reserved anything,
// grouped by agent/resource/tier) instead of one query per bucket per agent.
func PopulateUsage(ctx context.Context, dbURL, platformExpID string, quotas []*domain.AgentQuota) error {
	if len(quotas) == 0 {
		return nil
	}
	promQL := fmt.Sprintf(`sum by (agent_id, resource_type, tier) (%s{platform_experiment_id=%q})`, usedHoursMetric, platformExpID)
	samples, err := QueryVector(ctx, dbURL, promQL)
	if err != nil {
		return fmt.Errorf("metricsdb.PopulateUsage: %w", err)
	}

	byAgent := make(map[string]*domain.AgentQuota, len(quotas))
	for _, q := range quotas {
		byAgent[q.AgentID] = q
	}

	for _, s := range samples {
		q, ok := byAgent[s.Labels["agent_id"]]
		if !ok {
			continue
		}
		applyUsedSample(q, domain.ResourceType(s.Labels["resource_type"]), domain.CapacityTier(s.Labels["tier"]), s.Value)
	}
	return nil
}

// PopulateUsageOne fills a single quota's Used* fields from the metrics DB, summed across every
// job the agent has run in this platform experiment.
func PopulateUsageOne(ctx context.Context, dbURL string, q *domain.AgentQuota) error {
	if q == nil {
		return nil
	}
	promQL := fmt.Sprintf(`sum by (resource_type, tier) (%s{agent_id=%q, platform_experiment_id=%q})`, usedHoursMetric, q.AgentID, q.PlatformExperimentID)
	samples, err := QueryVector(ctx, dbURL, promQL)
	if err != nil {
		return fmt.Errorf("metricsdb.PopulateUsageOne: %w", err)
	}
	for _, s := range samples {
		applyUsedSample(q, domain.ResourceType(s.Labels["resource_type"]), domain.CapacityTier(s.Labels["tier"]), s.Value)
	}
	return nil
}

func applyUsedSample(q *domain.AgentQuota, resourceType domain.ResourceType, tier domain.CapacityTier, value float64) {
	guaranteed := tier == domain.CapacityGuaranteed
	switch resourceType {
	case domain.ResourceCPUCoreHours:
		if guaranteed {
			q.UsedGuaranteedCPUCoreH = value
		} else {
			q.UsedBurstCPUCoreH = value
		}
	case domain.ResourceRAMGBHours:
		if guaranteed {
			q.UsedGuaranteedRAMGBH = value
		} else {
			q.UsedBurstRAMGBH = value
		}
	case domain.ResourceStorageGBHours:
		if guaranteed {
			q.UsedGuaranteedStorageGBH = value
		} else {
			q.UsedBurstStorageGBH = value
		}
	default: // domain.ResourceGPUHours
		if guaranteed {
			q.UsedGuaranteedT4H = value
		} else {
			q.UsedBurstT4H = value
		}
	}
}

// TotalObservedT4H sums the settled, observed GPU cost across every agent and job in a platform
// experiment — filtered to kind=observed, so it counts only jobs that have actually settled and
// never a still-queued or running job's reservation. This is the "committed" half of the phase-2
// boundary check; the caller adds live actual usage of running attempts on top (see
// controller.checkPhase2Transition). Reading reservations here would let a large queued job
// prematurely trip phase 2 and cancel work.
func TotalObservedT4H(ctx context.Context, dbURL, platformExpID string) (float64, error) {
	promQL := fmt.Sprintf(`sum(%s{platform_experiment_id=%q, resource_type=%q, kind=%q})`, usedHoursMetric, platformExpID, string(domain.ResourceGPUHours), kindObserved)
	samples, err := QueryVector(ctx, dbURL, promQL)
	if err != nil {
		return 0, fmt.Errorf("metricsdb.TotalObservedT4H: %w", err)
	}
	if len(samples) == 0 {
		return 0, nil
	}
	return samples[0].Value, nil
}
