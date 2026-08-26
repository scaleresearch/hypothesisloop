package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
)

// Store embeds all store types providing unified access to all
// persistence operations.
type Store struct {
	*ExperimentsStore
	*AgentsStore
	*DonationStore
	*PlatformExperimentsStore
	*ClusterQueueStore
	*HypothesesStore
	*ClusterSettingsStore

	// usage is the sole read/write path for agent quota consumption (used_guaranteed_*/
	// used_burst_*), backed by the metrics DB rather than Postgres — see metricsdb.UsageTracker.
	usage *metricsdb.UsageTracker

	// maxInfraRequeues bounds how many times one experiment may be returned to the queue for
	// free after an infrastructure fault, before ResolveTermination gives up and terminates it
	// with the reason that keeps ending it. Without a ceiling a job landing repeatedly on broken
	// hardware loops forever, saying nothing.
	maxInfraRequeues int
}

// NewStore creates a Store backed by the given pool, tracking agent quota usage in the metrics
// DB at metricsDBURL. maxInfraRequeues must be positive: there is no "unbounded" and no "off" —
// a zero would silently turn every infrastructure fault back into a terminal eviction.
func NewStore(pool *Pool, metricsDBURL string, maxInfraRequeues int) *Store {
	if maxInfraRequeues <= 0 {
		panic("db.NewStore: max_infrastructure_requeues must be positive")
	}
	return &Store{
		maxInfraRequeues:         maxInfraRequeues,
		ExperimentsStore:         NewExperimentsStore(pool),
		AgentsStore:              NewAgentsStore(pool),
		DonationStore:            NewDonationStore(pool),
		PlatformExperimentsStore: NewPlatformExperimentsStore(pool),
		ClusterQueueStore:        NewClusterQueueStore(pool),
		HypothesesStore:          NewHypothesesStore(pool),
		ClusterSettingsStore:     NewClusterSettingsStore(pool),
		usage:                    metricsdb.NewUsageTracker(metricsDBURL),
	}
}

// ---- Loop adapters ----

// LoopQuotaStore wraps PlatformExperimentsFullStore and satisfies scheduler.LoopQuotaStore.
type LoopQuotaStore struct{ *PlatformExperimentsFullStore }

// NewLoopQuotaStore returns a LoopQuotaStore.
func NewLoopQuotaStore(s *PlatformExperimentsFullStore) *LoopQuotaStore {
	return &LoopQuotaStore{PlatformExperimentsFullStore: s}
}

// GetAgentQuota overrides the embedded PlatformExperimentsStore's allocation-only read, merging
// in current usage from the metrics DB — the scheduler loop's fairness sort needs both.
func (lq *LoopQuotaStore) GetAgentQuota(ctx context.Context, agentID, platformExpID string) (*domain.AgentQuota, error) {
	aq, err := lq.PlatformExperimentsStore.GetAgentQuota(ctx, agentID, platformExpID)
	if err != nil || aq == nil {
		return aq, err
	}
	pe, err := lq.PlatformExperimentsStore.GetPlatformExperiment(ctx, platformExpID)
	if err != nil {
		return nil, err
	}
	if pe == nil {
		return nil, fmt.Errorf("db.LoopQuotaStore.GetAgentQuota: platform experiment %s not found", platformExpID)
	}
	if err := metricsdb.PopulateUsageOne(ctx, lq.Store.usage.URL(), pe.CreatedAt, aq); err != nil {
		return nil, err
	}
	if err := lq.PlatformExperimentsStore.AddDesiredQuotaUsageOne(ctx, aq); err != nil {
		return nil, err
	}
	return aq, nil
}

// ---- Platform Experiments adapter ----

// PlatformExperimentsFullStore embeds Store and satisfies quota.PlatformExperimentsStore
// by providing both platform experiment and agent methods from a single composite.
type PlatformExperimentsFullStore struct {
	*Store
}

// NewPlatformExperimentsFullStore returns a store satisfying quota.PlatformExperimentsStore.
func NewPlatformExperimentsFullStore(s *Store) *PlatformExperimentsFullStore {
	return &PlatformExperimentsFullStore{Store: s}
}

// AdmitExperimentTx serializes, validates, and inserts one desired experiment in a single
// PostgreSQL transaction. observed contains only the metrics-store snapshot taken immediately
// before this call. An empty rejection reason means success; database failures are returned as
// errors and must never be presented as quota rejection.
// AdmitDecision is what AdmitExperimentTx concluded about one submission. The existence read that
// produces it happens inside the transaction, under the same lock as quota validation, so there is
// exactly one place that decides whether an id is new, a legal re-submission, or taken.
type AdmitDecision int

const (
	// AdmitInserted: the id was free and the desired-state row now exists.
	AdmitInserted AdmitDecision = iota
	// AdmitAlreadyQueued: the id is already QUEUED. Re-submitting a queued job is legal and only
	// refreshes its priority -- a state a unique constraint cannot express, which is why this
	// decision is read rather than inferred from a failed insert.
	AdmitAlreadyQueued
	// AdmitRejected: nothing was written; the rejection reason says why.
	AdmitRejected
)

// RejectionRateLimited prefixes the rejection reason AdmitExperimentTx returns when the
// submission rate limit is what refused the experiment, so a caller can tell that apart from a
// quota rejection without matching on prose.
const RejectionRateLimited = "rate_limited:"

// SubmissionRateLimit bounds how many experiments one agent may submit into one platform
// experiment within Window. MaxPerHour <= 0 means unlimited.
type SubmissionRateLimit struct {
	MaxPerHour int
	Window     time.Duration
}

func (s *PlatformExperimentsFullStore) AdmitExperimentTx(ctx context.Context, exp *domain.Experiment, observe func(context.Context) (*domain.AgentQuota, error), rateLimit SubmissionRateLimit) (AdmitDecision, string, error) {
	tx, err := s.PlatformExperimentsStore.pool.pool.Begin(ctx)
	if err != nil {
		return AdmitRejected, "", fmt.Errorf("admit experiment: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	key := exp.AgentID + "/" + exp.PlatformExperimentID
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, key); err != nil {
		return AdmitRejected, "", fmt.Errorf("admit experiment: lock: %w", err)
	}
	// The one place that decides whether this id is new, a legal re-submission, or taken. It is
	// read here rather than before the transaction because only here is it atomic with the insert
	// below, under the same lock. The unique constraint on the primary key remains, but it is an
	// invariant backstop -- reaching it means two different agents raced the same id, which is a
	// collision, not a retry -- and no longer a second place that decides "duplicate".
	var existingStatus, existingAgent, existingPE string
	err = tx.QueryRow(ctx, `SELECT status, agent_id, platform_experiment_id FROM experiments WHERE id = $1`,
		exp.ID).Scan(&existingStatus, &existingAgent, &existingPE)
	switch {
	case err == nil && (existingAgent != exp.AgentID || existingPE != exp.PlatformExperimentID):
		// Someone else's job. Ownership is checked, not just status: this transaction holds the
		// lock for *this* submitter's (agent, platform experiment), so it can say nothing about
		// another owner's row -- and treating one as a re-submission would let any agent refresh
		// the priority of a job it does not own by guessing an id.
		return AdmitRejected, "", fmt.Errorf("%w: %s belongs to another agent", ErrDuplicateExperiment, exp.ID)
	case err == nil && domain.ExperimentStatus(existingStatus) == domain.StatusQueued:
		return AdmitAlreadyQueued, "", nil
	case err == nil:
		return AdmitRejected, "", fmt.Errorf("%w: %s already exists with status %s", ErrDuplicateExperiment, exp.ID, existingStatus)
	case !errors.Is(err, pgx.ErrNoRows):
		return AdmitRejected, "", fmt.Errorf("admit experiment: existing row: %w", err)
	}

	observedCtx, cancelObserved := context.WithTimeout(ctx, InTxObservationTimeout)
	observed, err := observe(observedCtx)
	cancelObserved()
	if err != nil {
		return AdmitRejected, "", fmt.Errorf("admit experiment: observed usage: %w", err)
	}

	// Counted here rather than before the transaction: the advisory lock above is per
	// (agent, platform experiment), which is exactly the scope this limit is expressed in, so
	// inside it the count cannot go stale. Outside it, N concurrent submissions all read the
	// same pre-limit count and all pass.
	if rateLimit.MaxPerHour > 0 {
		var recent int
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM experiments
			WHERE agent_id = $1 AND platform_experiment_id = $2 AND created_at >= $3`,
			exp.AgentID, exp.PlatformExperimentID, time.Now().UTC().Add(-rateLimit.Window)).Scan(&recent); err != nil {
			return AdmitRejected, "", fmt.Errorf("admit experiment: recent submissions: %w", err)
		}
		if recent >= rateLimit.MaxPerHour {
			return AdmitRejected, fmt.Sprintf("%s %d experiments submitted in the last hour (limit: %d)", RejectionRateLimited, recent, rateLimit.MaxPerHour), nil
		}
	}

	var cut bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM platform_experiment_cuts WHERE platform_experiment_id=$1 AND agent_id=$2
	)`, exp.PlatformExperimentID, exp.AgentID).Scan(&cut); err != nil {
		return AdmitRejected, "", fmt.Errorf("admit experiment: stage cut: %w", err)
	}
	if cut {
		return AdmitRejected, "agent was cut at a stage boundary", nil
	}

	allocation, err := scanAgentQuota(tx.QueryRow(ctx, `SELECT`+agentQuotaColumns+`
		FROM agent_quotas WHERE agent_id=$1 AND platform_experiment_id=$2 FOR UPDATE`, exp.AgentID, exp.PlatformExperimentID))
	if err == pgx.ErrNoRows {
		return AdmitRejected, "no quota found (agent not signed up?)", nil
	}
	if err != nil {
		return AdmitRejected, "", fmt.Errorf("admit experiment: quota: %w", err)
	}

	var desiredGuaranteedAcc, desiredBurstAcc float64
	if err := tx.QueryRow(ctx, `
		SELECT
			COALESCE(SUM(estimated_cost_acch) FILTER (WHERE capacity_tier='guaranteed'), 0),
			COALESCE(SUM(estimated_cost_acch) FILTER (WHERE capacity_tier='burst'), 0)
		FROM experiments
		WHERE agent_id=$1 AND platform_experiment_id=$2
		  AND (status IN ('QUEUED','SUBMITTED','ADMITTED','RUNNING')
		       OR (status IN ('COMPLETED','FAILED','EVICTED','REJECTED') AND quota_settled_at IS NULL))`,
		exp.AgentID, exp.PlatformExperimentID).Scan(
		&desiredGuaranteedAcc, &desiredBurstAcc); err != nil {
		return AdmitRejected, "", fmt.Errorf("admit experiment: desired usage: %w", err)
	}

	guaranteed := exp.CapacityTier == domain.CapacityGuaranteed
	usedAcc, limitAcc := observed.UsedBurstAccH+desiredBurstAcc, allocation.BurstAcceleratorHours
	if guaranteed {
		usedAcc, limitAcc = observed.UsedGuaranteedAccH+desiredGuaranteedAcc, allocation.GuaranteedAcceleratorHours
	}
	if exp.EstimatedCostAccH > 0 && usedAcc+exp.EstimatedCostAccH > limitAcc {
		return AdmitRejected, fmt.Sprintf("insufficient_%s_quota: need %.2f accelerator_hours, have %.2f remaining", exp.CapacityTier, exp.EstimatedCostAccH, limitAcc-usedAcc), nil
	}
	if err := createExperiment(ctx, tx, exp); err != nil {
		return AdmitRejected, "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return AdmitRejected, "", fmt.Errorf("admit experiment: commit: %w", err)
	}
	return AdmitInserted, "", nil
}

// ReserveAdmittedFlavorTx rechecks the selected flavor's estimate and persists it under the
// same cross-replica quota lock used by initial admission.
func (s *PlatformExperimentsFullStore) ReserveAdmittedFlavorTx(ctx context.Context, experimentID string, acceleratorType domain.AcceleratorType, estimatedCost float64, observe func(context.Context, string, string) (*domain.AgentQuota, error)) (string, error) {
	tx, err := s.PlatformExperimentsStore.pool.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("reserve admitted flavor: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var agentID, platformExpID string
	var tier domain.CapacityTier
	var oldCost float64
	if err := tx.QueryRow(ctx, `SELECT agent_id, platform_experiment_id, capacity_tier, estimated_cost_acch FROM experiments WHERE id=$1`, experimentID).
		Scan(&agentID, &platformExpID, &tier, &oldCost); err != nil {
		return "", fmt.Errorf("reserve admitted flavor: experiment: %w", err)
	}
	key := agentID + "/" + platformExpID
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, key); err != nil {
		return "", fmt.Errorf("reserve admitted flavor: lock: %w", err)
	}
	observedCtx, cancelObserved := context.WithTimeout(ctx, InTxObservationTimeout)
	observed, err := observe(observedCtx, agentID, platformExpID)
	cancelObserved()
	if err != nil {
		return "", fmt.Errorf("reserve admitted flavor: observed usage: %w", err)
	}
	allocation, err := scanAgentQuota(tx.QueryRow(ctx, `SELECT`+agentQuotaColumns+`
		FROM agent_quotas WHERE agent_id=$1 AND platform_experiment_id=$2 FOR UPDATE`, agentID, platformExpID))
	if err != nil {
		return "", fmt.Errorf("reserve admitted flavor: quota: %w", err)
	}
	var desired float64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(estimated_cost_acch),0) FROM experiments
		WHERE agent_id=$1 AND platform_experiment_id=$2 AND capacity_tier=$3
		AND (status IN ('QUEUED','SUBMITTED','ADMITTED','RUNNING')
		OR (status IN ('COMPLETED','FAILED','EVICTED','REJECTED') AND quota_settled_at IS NULL))`,
		agentID, platformExpID, tier).Scan(&desired); err != nil {
		return "", fmt.Errorf("reserve admitted flavor: desired usage: %w", err)
	}
	used, limit := observed.UsedBurstAccH, allocation.BurstAcceleratorHours
	if tier == domain.CapacityGuaranteed {
		used, limit = observed.UsedGuaranteedAccH, allocation.GuaranteedAcceleratorHours
	}
	projected := used + desired - oldCost + estimatedCost
	if projected > limit {
		return fmt.Sprintf("insufficient_%s_quota for selected %s: need %.2f additional accelerator_hours, have %.2f remaining", tier, acceleratorType, estimatedCost-oldCost, limit-(used+desired-oldCost)), nil
	}
	if _, err := tx.Exec(ctx, `UPDATE experiments SET accelerator_type=$2, estimated_cost_acch=$3, updated_at=NOW() WHERE id=$1`, experimentID, string(acceleratorType), estimatedCost); err != nil {
		return "", fmt.Errorf("reserve admitted flavor: update: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("reserve admitted flavor: commit: %w", err)
	}
	return "", nil
}
