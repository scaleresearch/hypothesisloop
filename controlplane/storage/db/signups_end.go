package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// RemoveAgent takes an agent out of a platform experiment that has not started. ErrSignupClosed
// once it has, domain.ErrNotSignedUp when the agent is not in it, ErrWorkUnfinished while a job of it is
// queued or running, which only an agent of role baseline has then. Only its place goes, and the quota a
// baseline agent held with it: the agent itself
// stays in its org however it came to be, as every agent that ever joined does, and can be added
// again by its id. The experiment row is locked as in
// enrol, so a removal and a start cannot both believe they came first. The signup holds the only
// record of the agent's object-store key, so deleteDataKey takes that key out of the store before
// the row goes, and a key that could not be deleted keeps the signup.
func (s *Store) RemoveAgent(ctx context.Context, platformExpID, agentID string, deleteDataKey func(ctx context.Context, accessKeyID string) error) error {
	return s.write(ctx, func(tx *Write) error {
		var err error
		// A submission of this agent in flight commits or rolls back first, as in FinishAgent, so the
		// jobs read below include it, and one that starts later finds no quota to be admitted on.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, admissionLockKey(agentID, platformExpID)); err != nil {
			return fmt.Errorf("platform_experiments_store.RemoveAgent: lock: %w", err)
		}
		var status string
		var platformRuns bool
		if err := tx.QueryRow(ctx, `SELECT status, coordinator_hosted FROM platform_experiments WHERE id=$1 FOR UPDATE`, platformExpID).Scan(&status, &platformRuns); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrSignupClosed
			}
			return fmt.Errorf("platform_experiments_store.RemoveAgent: lock experiment: %w", err)
		}
		if status != string(domain.PlatformExpOpen) {
			return ErrSignupClosed
		}
		var accessKeyID string
		var inFlight bool
		err = tx.QueryRow(ctx, `
SELECT s.data_access_key_id,
       EXISTS (SELECT 1 FROM experiments
               WHERE platform_experiment_id = s.platform_experiment_id AND agent_id = s.agent_id AND status IN ('QUEUED', 'SUBMITTED', 'RUNNING'))
FROM experiment_signups s
WHERE s.platform_experiment_id=$1 AND s.agent_id=$2 FOR UPDATE OF s`, platformExpID, agentID).Scan(&accessKeyID, &inFlight)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrNotSignedUp
		}
		if err != nil {
			return fmt.Errorf("platform_experiments_store.RemoveAgent: read signup: %w", err)
		}
		// The agent's quota goes with its place, and a job of it that still runs is charged to that
		// quota by every pass of the controller: the job ends first.
		if inFlight {
			return ErrWorkUnfinished
		}
		if accessKeyID != "" {
			if err := deleteDataKey(ctx, accessKeyID); err != nil {
				return fmt.Errorf("platform_experiments_store.RemoveAgent: delete data key: %w", err)
			}
		}
		keys, err := signupKeysTx(ctx, tx, platformExpID, []string{agentID})
		if err != nil {
			return fmt.Errorf("platform_experiments_store.RemoveAgent: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM hosted_instances WHERE platform_experiment_id=$1 AND agent_id=$2`, platformExpID, agentID); err != nil {
			return fmt.Errorf("platform_experiments_store.RemoveAgent: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM experiment_signups WHERE platform_experiment_id=$1 AND agent_id=$2`, platformExpID, agentID); err != nil {
			return fmt.Errorf("platform_experiments_store.RemoveAgent: %w", err)
		}
		// Where the platform runs the researchers, the pool is what it runs: a researcher removed
		// before the start lowers its entry, or the platform would add another in its place.
		if platformRuns && len(keys) > 0 {
			if err := shrinkPoolToAtWorkTx(ctx, tx, platformExpID, keys[0]); err != nil {
				return fmt.Errorf("platform_experiments_store.RemoveAgent: %w", err)
			}
		}
		// Only an agent of role baseline holds one before the start.
		if _, err := tx.Exec(ctx, `DELETE FROM agent_quotas WHERE platform_experiment_id=$1 AND agent_id=$2`, platformExpID, agentID); err != nil {
			return fmt.Errorf("platform_experiments_store.RemoveAgent: release quota: %w", err)
		}
		return nil
	})
}

// ErrWorkUnfinished refuses finishing or removing an agent that still has a job queued or
// running.
var ErrWorkUnfinished = errors.New("the agent has a job in flight")

// FinishAgent records that an agent of a running platform experiment has nothing left to do, or
// that one that measures the baseline is done in an open one, where its work is. It answers
// ErrSignupClosed anywhere else, domain.ErrNotSignedUp, and ErrWorkUnfinished. An agent that already
// finished keeps the moment and the reason it has.
func (s *Store) FinishAgent(ctx context.Context, platformExpID, agentID, reason string) error {
	return s.write(ctx, func(tx *Write) error {
		var err error
		// A submission of this agent in flight commits or rolls back first, so the jobs read below
		// include it, and one that starts later reads the finish.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, admissionLockKey(agentID, platformExpID)); err != nil {
			return fmt.Errorf("platform_experiments_store.FinishAgent: lock: %w", err)
		}
		var running, unfinished bool
		var finishedAt *time.Time
		err = tx.QueryRow(ctx, `
SELECT pe.status = 'running' OR pe.status = 'open' AND s.role = '`+string(domain.SignupRoleBaseline)+`', s.finished_at,
       EXISTS (SELECT 1 FROM experiments
               WHERE platform_experiment_id = s.platform_experiment_id AND agent_id = s.agent_id
                 AND status IN ('QUEUED', 'SUBMITTED', 'RUNNING'))
FROM experiment_signups s JOIN platform_experiments pe ON pe.id = s.platform_experiment_id
WHERE s.platform_experiment_id = $1 AND s.agent_id = $2
FOR UPDATE OF s`, platformExpID, agentID).Scan(&running, &finishedAt, &unfinished)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return domain.ErrNotSignedUp
		case err != nil:
			return fmt.Errorf("platform_experiments_store.FinishAgent: %w", err)
		case finishedAt != nil:
			return nil
		case !running:
			return ErrSignupClosed
		case unfinished:
			return ErrWorkUnfinished
		}
		if _, err := tx.Exec(ctx, `
UPDATE experiment_signups SET finished_at = now(), finished_reason = $3, finished_xid = pg_current_xact_id()::text::bigint
WHERE platform_experiment_id = $1 AND agent_id = $2`, platformExpID, agentID, reason); err != nil {
			return fmt.Errorf("platform_experiments_store.FinishAgent: %w", err)
		}
		return nil
	})
}

// SetCloseAgents keeps lines as how each agent's run of the platform experiment id stands at its
// close, for its record of the close. It is taken just before the close is written, and one that
// is closed keeps what it has.
func (s *Store) SetCloseAgents(ctx context.Context, id, lines string) error {
	if _, err := s.pool.pool.Exec(ctx, `UPDATE platform_experiments SET close_agents = $2 WHERE id = $1 AND status <> 'closed'`, id, lines); err != nil {
		return fmt.Errorf("platform_experiments_store.SetCloseAgents: %w", err)
	}
	return nil
}

// CloseAgents is what SetCloseAgents kept of the platform experiment id, empty when nothing was.
func (s *Store) CloseAgents(ctx context.Context, id string) (string, error) {
	var lines string
	if err := s.pool.pool.QueryRow(ctx, `SELECT close_agents FROM platform_experiments WHERE id = $1`, id).Scan(&lines); err != nil {
		return "", fmt.Errorf("platform_experiments_store.CloseAgents: %w", err)
	}
	return lines, nil
}
