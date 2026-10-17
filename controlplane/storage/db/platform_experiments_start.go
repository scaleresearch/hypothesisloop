package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// StageRedistribution describes one resource dimension's move across a stage boundary: the cut
// agents' unspent allocation, plus the share of the platform budget withheld for the incoming
// stage, split evenly across survivors (domain.StageCredit).
//
// UsedByAgent carries observed consumption per cut agent, which is metrics-store data and so
// cannot be read under the Postgres lock. That is safe in a way the allocation is not: observed
// usage only ever moves up, whereas an allocation can move in either direction, and
// deriving "unspent" from an allocation read before the lock mints or destroys hours relative to
// the platform budget. The allocation side is therefore read inside the transaction.
type StageRedistribution struct {
	ResourceType domain.ResourceType
	Budget       float64
	ReleaseFrac  float64
	UsedByAgent  map[string]float64
	// UsedBurstByAgent is the same observed figure for burst-tier work, which is where a
	// burst-only agent's whole allocation and consumption live.
	UsedBurstByAgent map[string]float64
}

// StartParticipant is one signed-up agent and everything the allocation needs to know about it,
// resolved inside the start transaction.
type StartParticipant struct {
	AgentID string
	// Role says whether the agent is a researcher, which the run's ladder is sized by.
	Role        domain.SignupRole
	AgentExists bool
	HasTop3     bool
	// QuotaTier decides which column this participant's share lands in. Read here, inside the
	// start transaction, because the signup set is: reading the tier from a separate query could
	// pair one transaction's participants with another's tiers.
	QuotaTier domain.QuotaTier
}

// sharesBudgetSQL is domain.AgentSignup.SharesBudget of the signup s: a researcher or any other
// agent the start gives a share.
const sharesBudgetSQL = `s.role NOT IN ('` + string(domain.SignupRoleBaseline) + `', '` + string(domain.SignupRolePlanner) + `')`

// runOverAtSQL is the end of the run of the running pe: duration_hours after its starts_at, the
// moment the run started.
const runOverAtSQL = `pe.starts_at + make_interval(secs => pe.duration_hours * 3600)`

// startClock records that a running platform experiment that waits for a researcher's machine
// has one at work, once the machine asked for under name for agentID is reported running:
// started_at is now, and it closes at the end of its run. It writes nothing for one that has a
// researcher at work.
func (s *Store) startClock(ctx context.Context, platformExpID, agentID, name string) error {
	if _, err := s.pool.pool.Exec(ctx, `
UPDATE platform_experiments pe
SET started_at = NOW(), ends_at = `+runOverAtSQL+`, updated_at = NOW()
WHERE pe.id = $1 AND pe.status = 'running' AND pe.started_at IS NULL
  AND EXISTS (SELECT 1 FROM experiment_signups s JOIN agentsandbox_instances i USING (platform_experiment_id, agent_id)
              WHERE s.platform_experiment_id = pe.id AND s.agent_id = $2 AND s.in_agentsandbox AND i.name = $3
                AND i.state = '`+domain.InstanceRunning+`' AND `+sharesBudgetSQL+`)`,
		platformExpID, agentID, name); err != nil {
		return fmt.Errorf("platform_experiments_store.startClock: %w", err)
	}
	return nil
}

// StartPlatformExperimentTx flips a platform experiment open->running and writes every
// participating agent's quota and the ladder the run has (domain.RunStages) in one transaction.
// The run starts now: starts_at is now, and it ends duration_hours later whatever measuring the
// baseline took. With a researcher its owner runs, started_at is now and it closes at that end.
// With only researchers the platform runs, started_at stays empty until the first one's machine
// is reported running (startClock), and it closes agentsDue from now, or at that end when sooner:
// the sweep closes one whose machines never came. quotasFor is
// called with the signup set read inside that transaction; returning an error rolls the whole
// thing back, leaving the experiment open and the call safe to retry.
//
// Atomic because the two halves used to be separate: the signup list was read, quotas were
// written, and only then was the status flipped. An agent signing up in that window was durably
// signed up with no quota row and no path to ever get one. Flipping the status inside the same
// transaction closes the window from the other side too — Signup only inserts while the
// experiment is still open, so a signup either lands before this transaction and is seen by it,
// or fails.
//
// Returns started=false when the experiment was not open.
func (s *Store) StartPlatformExperimentTx(ctx context.Context, id string, agentsDue time.Duration, quotasFor func(participants []StartParticipant) ([]*domain.AgentQuota, []domain.Stage, error)) (bool, []*domain.AgentQuota, error) {
	var quotas []*domain.AgentQuota
	started := false
	if err := s.write(ctx, func(tx *Write) error {
		moved, err := moveRun(ctx, tx, `
WITH at_work AS (
    SELECT EXISTS (SELECT 1 FROM experiment_signups s
                   WHERE s.platform_experiment_id = $1 AND NOT s.in_agentsandbox AND `+sharesBudgetSQL+`) AS already
)
UPDATE platform_experiments pe
SET status='running', updated_at=NOW(), starts_at = NOW(), `+statusMoved+`,
    started_at = CASE WHEN at_work.already THEN NOW() END,
    ends_at = NOW() + make_interval(secs => CASE WHEN at_work.already THEN pe.duration_hours * 3600 ELSE LEAST($2, pe.duration_hours * 3600) END)
FROM at_work
WHERE pe.id=$1 AND pe.status='open'`, id, agentsDue.Seconds())
		if err != nil {
			return fmt.Errorf("platform_experiments_store.StartPlatformExperimentTx: claim: %w", err)
		}
		if !moved {
			return nil
		}

		// An agent of role baseline takes no share of the run: it measured the baseline before the
		// start, against the quota it joined with. A draft's planner took part in the draft only.
		// Signups joined against the agents they name and their top-3 history, all on this
		// transaction's own connection. The caller must not issue its own queries while this
		// transaction is open: several concurrent starts would each hold a connection and wait for
		// another from the same pool, which is a deadlock rather than a slow path.
		rows, err := tx.Query(ctx, `
SELECT s.agent_id, s.role, a.id IS NOT NULL, EXISTS (SELECT 1 FROM experiment_top3 t WHERE t.agent_id = s.agent_id),
       s.quota_tier
FROM experiment_signups s
LEFT JOIN agents a ON a.id = s.agent_id
WHERE s.platform_experiment_id = $1 AND `+sharesBudgetSQL+`
ORDER BY s.agent_id`, id)
		if err != nil {
			return fmt.Errorf("platform_experiments_store.StartPlatformExperimentTx: list signups: %w", err)
		}
		var participants []StartParticipant
		for rows.Next() {
			var p StartParticipant
			if err := rows.Scan(&p.AgentID, &p.Role, &p.AgentExists, &p.HasTop3, &p.QuotaTier); err != nil {
				rows.Close()
				return fmt.Errorf("platform_experiments_store.StartPlatformExperimentTx: scan signup: %w", err)
			}
			participants = append(participants, p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("platform_experiments_store.StartPlatformExperimentTx: list signups: %w", err)
		}

		var stages []domain.Stage
		quotas, stages, err = quotasFor(participants)
		if err != nil {
			return err
		}
		if err := domain.ValidateStages(stages); err != nil {
			return fmt.Errorf("platform_experiments_store.StartPlatformExperimentTx: %w", err)
		}
		stagesJSON, err := json.Marshal(stages)
		if err != nil {
			return fmt.Errorf("platform_experiments_store.StartPlatformExperimentTx: marshal stages: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE platform_experiments SET stages=$2 WHERE id=$1`, id, stagesJSON); err != nil {
			return fmt.Errorf("platform_experiments_store.StartPlatformExperimentTx: store stages: %w", err)
		}
		for _, q := range quotas {
			if err := upsertAgentQuota(ctx, tx, q); err != nil {
				return fmt.Errorf("platform_experiments_store.StartPlatformExperimentTx: upsert quota for %s: %w", q.AgentID, err)
			}
		}
		started = true
		return nil
	}); err != nil {
		return false, nil, err
	}
	return started, quotas, nil
}

// AdvanceStage commits one stage boundary in a single Postgres transaction: it claims
// stageIndex in platform_experiment_stage_advances, records the cut agents, applies every
// zero/add quota op, and moves current_stage to stageIndex+1.
//
// All of it is one commit because quota moves are the only part that cannot be retried: re-running
// an add's delta would double-credit an agent, and there is no idempotent way to retry a
// partially-applied delta. Any crash before commit leaves the advance row absent and nothing
// applied, so the next reconcile tick redoes the whole boundary safely from scratch; any crash
// after commit has already applied everything exactly once. Job-stopping for cut agents is
// naturally idempotent and lives outside this transaction, retried every tick.
//
// Returns (false, nil) if this boundary was already committed by an earlier call — the caller
// must skip straight to retrying job-stopping instead of re-applying ops.
func (s *Store) AdvanceStage(ctx context.Context, platformExpID string, stageIndex int, cutAgentIDs, survivorIDs []string, dims []StageRedistribution) (bool, error) {
	advanced := false
	if err := s.write(ctx, func(tx *Write) error {
		// The primary key on (platform_experiment_id, stage_index) is the claim: exactly one caller
		// inserts a row for this boundary, so concurrent control-service replicas reconciling the
		// same platform experiment can never both apply the ops.
		tag, err := tx.Exec(ctx,
			`INSERT INTO platform_experiment_stage_advances (platform_experiment_id, stage_index)
		 VALUES ($1, $2) ON CONFLICT DO NOTHING`,
			platformExpID, stageIndex,
		)
		if err != nil {
			return fmt.Errorf("platform_experiments_store.AdvanceStage: claim: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return nil // boundary already committed
		}
		tx.runChange(platformExpID, domain.EventStageBoundary, strconv.Itoa(stageIndex), "advanced")
		if err := cutTx(ctx, tx, platformExpID, cutAgentIDs, stageIndex, ""); err != nil {
			return fmt.Errorf("platform_experiments_store.AdvanceStage: %w", err)
		}
		// The stages after the boundary want as many fewer researchers as it cut, written with the
		// cut: nobody is added in a cut researcher's place.
		if len(cutAgentIDs) > 0 {
			if err := lowerPoolTx(ctx, tx, platformExpID, cutAgentIDs); err != nil {
				return fmt.Errorf("platform_experiments_store.AdvanceStage: %w", err)
			}
		}

		for _, dim := range dims {
			if dim.Budget <= 0 {
				continue // platform experiment does not track this dimension
			}
			guaranteed, burst := resourceQuotaColumns(dim.ResourceType)

			// Lock every cut agent's row and read its allocation here, not from a snapshot taken
			// before the transaction: a stage move committing in between would make "unspent" wrong in
			// either direction, minting or destroying hours against the platform budget.
			tiers, err := participantTiers(ctx, tx, platformExpID, cutAgentIDs)
			if err != nil {
				return fmt.Errorf("platform_experiments_store.AdvanceStage: cut agent tiers: %w", err)
			}
			allocated := make(map[string]float64, len(cutAgentIDs))
			used := make(map[string]float64, len(cutAgentIDs))
			lockQ := fmt.Sprintf(
				`SELECT agent_id, %s, %s FROM agent_quotas WHERE platform_experiment_id=$1 AND agent_id = ANY($2) ORDER BY agent_id FOR UPDATE`,
				guaranteed, burst)
			rows, err := tx.Query(ctx, lockQ, platformExpID, cutAgentIDs)
			if err != nil {
				return fmt.Errorf("platform_experiments_store.AdvanceStage: lock cut allocations: %w", err)
			}
			for rows.Next() {
				var agentID string
				var g, b float64
				if err := rows.Scan(&agentID, &g, &b); err != nil {
					rows.Close()
					return fmt.Errorf("platform_experiments_store.AdvanceStage: scan cut allocation: %w", err)
				}
				tier, ok := tiers[agentID]
				if !ok {
					rows.Close()
					return fmt.Errorf("platform_experiments_store.AdvanceStage: cut agent %s has no signup in %s", agentID, platformExpID)
				}
				// A burst-only agent holds its whole entitlement in the burst column (see
				// domain.ApplyQuotaTier), so that is the column its unspent hours are measured in.
				if tier == domain.QuotaTierGuaranteed {
					allocated[agentID], used[agentID] = g, dim.UsedByAgent[agentID]
				} else {
					allocated[agentID], used[agentID] = b, dim.UsedBurstByAgent[agentID]
				}
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return fmt.Errorf("platform_experiments_store.AdvanceStage: lock cut allocations: %w", err)
			}
			zeroQ := fmt.Sprintf(`UPDATE agent_quotas SET %s=0, %s=0, updated_at=NOW() WHERE platform_experiment_id=$1 AND agent_id = ANY($2)`, guaranteed, burst)
			if err := writeQuotas(ctx, tx, zeroQ, platformExpID, cutAgentIDs); err != nil {
				return fmt.Errorf("platform_experiments_store.AdvanceStage: zero %s: %w", dim.ResourceType, err)
			}
			if len(survivorIDs) == 0 {
				continue
			}
			credit := domain.StageCredit(dim.Budget, dim.ReleaseFrac, allocated, used, len(survivorIDs))
			if credit <= 0 {
				continue
			}
			if err := creditIntoTier(ctx, tx, platformExpID, survivorIDs, guaranteed, burst, credit, 0); err != nil {
				return fmt.Errorf("platform_experiments_store.AdvanceStage: add %s: %w", dim.ResourceType, err)
			}
		}

		if _, err := tx.Exec(ctx,
			`UPDATE platform_experiments SET current_stage=$2, updated_at=NOW() WHERE id=$1`,
			platformExpID, stageIndex+1,
		); err != nil {
			return fmt.Errorf("platform_experiments_store.AdvanceStage: bump current_stage: %w", err)
		}
		advanced = true
		return nil
	}); err != nil {
		return false, err
	}
	return advanced, nil
}

// ListCutAgents returns agent IDs cut at any stage boundary of a platform experiment,
// oldest cut first.
func (s *Store) ListCutAgents(ctx context.Context, platformExpID string) ([]domain.AgentCut, error) {
	const q = `SELECT agent_id, stage_index FROM platform_experiment_cuts WHERE platform_experiment_id=$1 ORDER BY cut_at ASC`
	rows, err := s.pool.pool.Query(ctx, q, platformExpID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.ListCutAgents: %w", err)
	}
	defer rows.Close()
	out := []domain.AgentCut{}
	for rows.Next() {
		var c domain.AgentCut
		if err := rows.Scan(&c.AgentID, &c.StageIndex); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListStageAdvances returns every boundary this platform experiment has crossed, in order.
func (s *Store) ListStageAdvances(ctx context.Context, platformExpID string) ([]domain.StageAdvance, error) {
	const q = `SELECT stage_index, advanced_at FROM platform_experiment_stage_advances WHERE platform_experiment_id=$1 ORDER BY stage_index ASC`
	rows, err := s.pool.pool.Query(ctx, q, platformExpID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.ListStageAdvances: %w", err)
	}
	defer rows.Close()
	out := []domain.StageAdvance{}
	for rows.Next() {
		var a domain.StageAdvance
		if err := rows.Scan(&a.StageIndex, &a.AdvancedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// IsAgentCut returns true if the agent was cut at a stage boundary of the given platform
// experiment. A cut is terminal for the rest of the experiment.
func (s *Store) IsAgentCut(ctx context.Context, platformExpID, agentID string) (bool, error) {
	const q = `SELECT 1 FROM platform_experiment_cuts WHERE platform_experiment_id=$1 AND agent_id=$2`
	var dummy int
	err := s.pool.pool.QueryRow(ctx, q, platformExpID, agentID).Scan(&dummy)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("platform_experiments_store.IsAgentCut: %w", err)
	}
	return true, nil
}

// AgentCutAt returns when the agent was cut from the platform experiment, nil when it was not.
func (s *Store) AgentCutAt(ctx context.Context, platformExpID, agentID string) (*time.Time, error) {
	var cutAt time.Time
	err := s.pool.pool.QueryRow(ctx,
		`SELECT cut_at FROM platform_experiment_cuts WHERE platform_experiment_id=$1 AND agent_id=$2`, platformExpID, agentID).Scan(&cutAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.AgentCutAt: %w", err)
	}
	return &cutAt, nil
}
