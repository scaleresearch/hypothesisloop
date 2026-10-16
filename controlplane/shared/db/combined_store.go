package db

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// Store embeds all store types providing unified access to all
// persistence operations.
type Store struct {
	*ExperimentsStore
	*AgentsStore
	*PlatformExperimentsStore
	*ClusterQueueStore
	*HypothesesStore
	*ClustersStore
	*EnvironmentsStore

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
		PlatformExperimentsStore: NewPlatformExperimentsStore(pool),
		ClusterQueueStore:        NewClusterQueueStore(pool),
		HypothesesStore:          NewHypothesesStore(pool),
		ClustersStore:            NewClustersStore(pool),
		EnvironmentsStore:        NewEnvironmentsStore(pool),
	}
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
// before this call. A submission it refuses is answered with an *AdmitRefusal. Database failures
// are any other error and must never be presented as a refusal.
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
	// AdmitRejected: nothing was written; the error says why.
	AdmitRejected
	// AdmitRepeated: the agent already submitted with this submission key. Nothing was written
	// and the experiment's ID is now that of the job the first submit created.
	AdmitRepeated
)

// AdmitRefusal is why AdmitExperimentTx wrote nothing for a submission it understood: the reason
// a client branches on and the sentence the submitter reads. It is the error of an AdmitRejected
// that is the submitter's to act on. Any other error is the platform's failure.
type AdmitRefusal struct {
	Reason  string
	Message string
}

func (r *AdmitRefusal) Error() string { return r.Reason + ": " + r.Message }

// The reasons of an AdmitRefusal, each answered to the submitter as it is.
const (
	RefusedRateLimited       = "rate_limited"
	RefusedAgentCut          = "agent_held"
	RefusedAgentFinished     = "agent_finished"
	RefusedNoQuota           = "no_quota"
	RefusedInsufficientQuota = "insufficient_credits"
)

// admissionLockKey names the advisory lock one agent's admissions in one platform experiment
// are decided under. FinishAgent takes it too, so a submission and a finish never interleave.
func admissionLockKey(agentID, platformExpID string) string {
	return agentID + "/" + platformExpID
}

// SubmissionRateLimit bounds how many experiments one agent may submit into one platform
// experiment within Window. MaxPerHour <= 0 means unlimited.
type SubmissionRateLimit struct {
	MaxPerHour int
	Window     time.Duration
}

// createDataKey registers the job's key with the object store after the row is written and
// before commit: a store failure rolls the submission back, and no key is created for an id that
// was rejected or already queued.
func (s *PlatformExperimentsFullStore) AdmitExperimentTx(ctx context.Context, exp *domain.Experiment, observe func(context.Context) (*domain.AgentQuota, error), rateLimit SubmissionRateLimit, createDataKey func(context.Context) error) (AdmitDecision, error) {
	tx, err := s.PlatformExperimentsStore.pool.pool.Begin(ctx)
	if err != nil {
		return AdmitRejected, fmt.Errorf("admit experiment: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, admissionLockKey(exp.AgentID, exp.PlatformExperimentID)); err != nil {
		return AdmitRejected, fmt.Errorf("admit experiment: lock: %w", err)
	}
	// A submit that was stored and whose answer was lost is sent again with its key. Read under
	// the agent's lock, so of two that race one inserts and the other finds its row here.
	if exp.SubmissionKey != "" {
		err := tx.QueryRow(ctx, `SELECT id FROM experiments WHERE platform_experiment_id = $1 AND agent_id = $2 AND submission_key = $3`,
			exp.PlatformExperimentID, exp.AgentID, exp.SubmissionKey).Scan(&exp.ID)
		if err == nil {
			return AdmitRepeated, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return AdmitRejected, fmt.Errorf("admit experiment: submission key: %w", err)
		}
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
		return AdmitRejected, fmt.Errorf("%w: %s belongs to another agent", ErrDuplicateExperiment, exp.ID)
	case err == nil && domain.ExperimentStatus(existingStatus) == domain.StatusQueued:
		return AdmitAlreadyQueued, nil
	case err == nil:
		return AdmitRejected, fmt.Errorf("%w: %s already exists with status %s", ErrDuplicateExperiment, exp.ID, existingStatus)
	case !errors.Is(err, pgx.ErrNoRows):
		return AdmitRejected, fmt.Errorf("admit experiment: existing row: %w", err)
	}

	observedCtx, cancelObserved := context.WithTimeout(ctx, InTxObservationTimeout)
	observed, err := observe(observedCtx)
	cancelObserved()
	if err != nil {
		return AdmitRejected, fmt.Errorf("admit experiment: observed usage: %w", err)
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
			return AdmitRejected, fmt.Errorf("admit experiment: recent submissions: %w", err)
		}
		if recent >= rateLimit.MaxPerHour {
			return AdmitRejected, &AdmitRefusal{Reason: RefusedRateLimited, Message: fmt.Sprintf("the agent submitted %d jobs to this platform experiment in the last hour, and %d is the most it may: submit again as the earliest of them turns an hour old", recent, rateLimit.MaxPerHour)}
		}
	}

	var cut bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM platform_experiment_cuts WHERE platform_experiment_id=$1 AND agent_id=$2
	)`, exp.PlatformExperimentID, exp.AgentID).Scan(&cut); err != nil {
		return AdmitRejected, fmt.Errorf("admit experiment: stage cut: %w", err)
	}
	if cut {
		return AdmitRejected, &AdmitRefusal{Reason: RefusedAgentCut, Message: "the agent was cut at a stage boundary and submits no further job in this platform experiment: GET /platform-experiments/{id}/stages says where it ranked"}
	}
	var finished bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM experiment_signups WHERE platform_experiment_id=$1 AND agent_id=$2 AND finished_at IS NOT NULL
	)`, exp.PlatformExperimentID, exp.AgentID).Scan(&finished); err != nil {
		return AdmitRejected, fmt.Errorf("admit experiment: finished: %w", err)
	}
	if finished {
		return AdmitRejected, &AdmitRefusal{Reason: RefusedAgentFinished, Message: "the agent finished in this platform experiment, which is final: it submits no further job"}
	}

	allocation, err := scanAgentQuota(tx.QueryRow(ctx, `SELECT`+agentQuotaColumns+`
		FROM agent_quotas WHERE agent_id=$1 AND platform_experiment_id=$2 FOR UPDATE`, exp.AgentID, exp.PlatformExperimentID))
	if err == pgx.ErrNoRows {
		return AdmitRejected, &AdmitRefusal{Reason: RefusedNoQuota, Message: "the agent holds no quota in this platform experiment, and a job spends its agent's quota: only an agent added to the platform experiment with a share of its budget submits jobs"}
	}
	if err != nil {
		return AdmitRejected, fmt.Errorf("admit experiment: quota: %w", err)
	}

	var desiredGuaranteedAcc, desiredBurstAcc float64
	var holding int
	if err := tx.QueryRow(ctx, `
		SELECT
			COALESCE(SUM(estimated_cost_acch) FILTER (WHERE capacity_tier='guaranteed'), 0),
			COALESCE(SUM(estimated_cost_acch) FILTER (WHERE capacity_tier='burst'), 0),
			COUNT(*) FILTER (WHERE estimated_cost_acch > 0)
		FROM experiments
		WHERE agent_id=$1 AND platform_experiment_id=$2
		  AND (status IN ('QUEUED','SUBMITTED','RUNNING')
		       OR (status IN ('COMPLETED','FAILED','EVICTED','REJECTED') AND quota_settled_at IS NULL))`,
		exp.AgentID, exp.PlatformExperimentID).Scan(
		&desiredGuaranteedAcc, &desiredBurstAcc, &holding); err != nil {
		return AdmitRejected, fmt.Errorf("admit experiment: desired usage: %w", err)
	}

	allocation.UsedGuaranteedAccH = observed.UsedGuaranteedAccH + desiredGuaranteedAcc
	allocation.UsedBurstAccH = observed.UsedBurstAccH + desiredBurstAcc
	remaining := allocation.Remaining(exp.CapacityTier)
	if exp.EstimatedDurationHours == 0 {
		if err := takeLargestEstimate(exp, remaining, holding, desiredGuaranteedAcc+desiredBurstAcc); err != nil {
			return AdmitRejected, err
		}
	} else if exp.EstimatedCostAccH > remaining {
		return AdmitRejected, &AdmitRefusal{Reason: RefusedInsufficientQuota, Message: quotaRefusal(exp, remaining, holding, desiredGuaranteedAcc+desiredBurstAcc)}
	}
	if err := createExperiment(ctx, tx, exp); err != nil {
		return AdmitRejected, err
	}
	if err := createDataKey(ctx); err != nil {
		return AdmitRejected, fmt.Errorf("admit experiment: durable-data key: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return AdmitRejected, fmt.Errorf("admit experiment: commit: %w", err)
	}
	return AdmitInserted, nil
}

// quotaRefusal is the rejection of a job whose estimate is over what its agent has remaining. It
// says what does fit, so the next submit is not a guess, and how much of what is used is only
// reserved.
func quotaRefusal(exp *domain.Experiment, remaining float64, holding int, reserved float64) string {
	return fmt.Sprintf("the %s job would need %.3g accelerator_hours (%d of %s for its estimated_duration_hours of %g), and the agent has %.3g remaining: guaranteed and burst jobs spend one share of the budget, and burst gives no hours on top of it. %s %s",
		exp.CapacityTier, exp.EstimatedCostAccH, exp.AcceleratorCount, exp.AcceleratorType, exp.EstimatedDurationHours, remaining,
		largestEstimateThatFits(exp, remaining), heldByEstimates(holding, reserved))
}

// fitSteps is how many steps of an hour the largest estimate that fits is rounded down to: one
// is 0.36 s.
const fitSteps = 1e4

// fitHours is the longest estimate a job holding perHour may have within remaining. A job's hold
// is its hours times what its size costs an hour, so the hours that fit are remaining over that,
// rounded down.
func fitHours(remaining, perHour float64) float64 {
	return math.Floor(remaining/perHour*fitSteps) / fitSteps
}

// takeLargestEstimate gives a job that reaches admission with no estimate, a baseline agent's,
// the largest its agent's quota admits, within exp.MaxEstimateHours. Nothing is measured before
// the baseline, so its agent has nothing to estimate from, and a job gives back what it does not
// spend when it ends. remaining is the figure every admission decides on, so the job fits it.
func takeLargestEstimate(exp *domain.Experiment, remaining float64, holding int, reserved float64) error {
	exp.EstimatedDurationHours = 1
	perHour, err := exp.EstimateHeld()
	if err != nil {
		return fmt.Errorf("admit experiment: %w", err)
	}
	hours := fitHours(remaining, perHour)
	if exp.MaxEstimateHours > 0 {
		hours = min(hours, exp.MaxEstimateHours)
	}
	if hours <= 0 {
		exp.EstimatedDurationHours = 0
		return &AdmitRefusal{Reason: RefusedInsufficientQuota, Message: fmt.Sprintf("the agent has %.3g accelerator_hours remaining, and a job of %d of %s holds %.3g an hour: no estimated_duration_hours fits it now. %s",
			remaining, exp.AcceleratorCount, exp.AcceleratorType, perHour, heldByEstimates(holding, reserved))}
	}
	exp.EstimatedDurationHours = hours
	exp.EstimatedCostAccH = perHour * hours
	return nil
}

// largestEstimateThatFits names the longest estimated_duration_hours a job of exp's size may
// state within remaining, rounded down so that stating them is admitted.
func largestEstimateThatFits(exp *domain.Experiment, remaining float64) string {
	fits := fitHours(remaining, exp.EstimatedCostAccH/exp.EstimatedDurationHours)
	if fits <= 0 {
		return "No estimated_duration_hours fits a job of this size now."
	}
	return fmt.Sprintf("The largest estimated_duration_hours a job of this size may state now is %s (%.0f s).", strconv.FormatFloat(fits, 'f', -1, 64), fits*3600)
}

// heldByEstimates tells a refused submitter how much of what is used is only reserved: a job
// holds its whole estimate until it ends and then only what it spent, so over-estimated jobs
// crowd out the next one though little has been spent.
func heldByEstimates(jobs int, reserved float64) string {
	if jobs == 0 {
		return "None of what is used is a reservation: it is spent."
	}
	return fmt.Sprintf("%d of your jobs that have not ended hold %.3g by their estimates and give back what they do not spend as each ends, so more fits once one does.", jobs, reserved)
}

// ReserveAdmittedFlavorTx rechecks the selected flavor's estimate, enforces the platform
// experiment's max_concurrent_accelerators spend-rate cap (autoscaler.md's concurrency cap:
// Σ accelerator_count over this pool's SUBMITTED+RUNNING rows + this job's count must not exceed
// the cap), and persists the flavor — all under the same cross-replica quota lock used by initial
// admission. defaultMaxConcurrent is quota.default_max_concurrent_accelerators, used when the
// platform experiment has no override.
func (s *PlatformExperimentsFullStore) ReserveAdmittedFlavorTx(ctx context.Context, experimentID string, acceleratorType domain.AcceleratorType, estimatedCost float64, acceleratorCount, defaultMaxConcurrent int, observe func(ctx context.Context, agentID, platformExpID string, platformExpStart time.Time) (*domain.AgentQuota, error)) (string, error) {
	tx, err := s.PlatformExperimentsStore.pool.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("reserve admitted flavor: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var agentID, platformExpID string
	var tier domain.CapacityTier
	var oldCost float64
	// The platform experiment's start bounds the usage lookback. It is read here, on the
	// transaction's connection, so observe needs no second one from the pool.
	var platformExpStart time.Time
	if err := tx.QueryRow(ctx, `SELECT e.agent_id, e.platform_experiment_id, e.capacity_tier, e.estimated_cost_acch, pe.created_at
		FROM experiments e JOIN platform_experiments pe ON pe.id = e.platform_experiment_id WHERE e.id=$1`, experimentID).
		Scan(&agentID, &platformExpID, &tier, &oldCost, &platformExpStart); err != nil {
		return "", fmt.Errorf("reserve admitted flavor: experiment: %w", err)
	}
	key := agentID + "/" + platformExpID
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, key); err != nil {
		return "", fmt.Errorf("reserve admitted flavor: lock: %w", err)
	}
	observedCtx, cancelObserved := context.WithTimeout(ctx, InTxObservationTimeout)
	observed, err := observe(observedCtx, agentID, platformExpID, platformExpStart)
	cancelObserved()
	if err != nil {
		return "", fmt.Errorf("reserve admitted flavor: observed usage: %w", err)
	}
	allocation, err := scanAgentQuota(tx.QueryRow(ctx, `SELECT`+agentQuotaColumns+`
		FROM agent_quotas WHERE agent_id=$1 AND platform_experiment_id=$2 FOR UPDATE`, agentID, platformExpID))
	if err != nil {
		return "", fmt.Errorf("reserve admitted flavor: quota: %w", err)
	}
	var desired, desiredAll float64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(estimated_cost_acch) FILTER (WHERE capacity_tier=$3),0), COALESCE(SUM(estimated_cost_acch),0) FROM experiments
		WHERE agent_id=$1 AND platform_experiment_id=$2
		AND (status IN ('QUEUED','SUBMITTED','RUNNING')
		OR (status IN ('COMPLETED','FAILED','EVICTED','REJECTED') AND quota_settled_at IS NULL))`,
		agentID, platformExpID, tier).Scan(&desired, &desiredAll); err != nil {
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
	if spent := observed.Used() + desiredAll - oldCost; spent+estimatedCost > allocation.Share() {
		return fmt.Sprintf("insufficient_%s_quota for selected %s: need %.2f additional accelerator_hours, the agent's share of the experiment budget has %.2f remaining", tier, acceleratorType, estimatedCost-oldCost, allocation.Share()-spent), nil
	}
	if acceleratorCount > 0 {
		var maxConcurrent *int
		if err := tx.QueryRow(ctx, `SELECT max_concurrent_accelerators FROM platform_experiments WHERE id=$1`, platformExpID).Scan(&maxConcurrent); err != nil {
			return "", fmt.Errorf("reserve admitted flavor: platform experiment cap: %w", err)
		}
		cap := defaultMaxConcurrent
		if maxConcurrent != nil {
			cap = *maxConcurrent
		}
		if cap > 0 {
			// The cap is defined on the platform experiment across ALL of its agents' jobs
			// (autoscaler.md, the migrations' max_concurrent_accelerators comment), not per agent —
			// so both the lock and the SUM below are scoped to platformExpID alone. A second,
			// distinctly-salted advisory lock is used here (rather than reusing the agent-scoped
			// `key` lock above) so two different agents under the same platform experiment still
			// serialize against each other instead of racing past this check independently.
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 1))`, platformExpID); err != nil {
				return "", fmt.Errorf("reserve admitted flavor: concurrency lock: %w", err)
			}
			var inFlight int
			if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(accelerator_count),0) FROM experiments
				WHERE platform_experiment_id=$1 AND id!=$2 AND status IN ('SUBMITTED','RUNNING')`,
				platformExpID, experimentID).Scan(&inFlight); err != nil {
				return "", fmt.Errorf("reserve admitted flavor: in-flight accelerators: %w", err)
			}
			if inFlight+acceleratorCount > cap {
				return domain.NotAdmittedConcurrencyCap, nil
			}
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE experiments SET accelerator_type=$2, estimated_cost_acch=$3 WHERE id=$1`, experimentID, string(acceleratorType), estimatedCost); err != nil {
		return "", fmt.Errorf("reserve admitted flavor: update: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("reserve admitted flavor: commit: %w", err)
	}
	return "", nil
}
