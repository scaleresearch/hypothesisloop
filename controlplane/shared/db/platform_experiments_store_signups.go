package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// ---- experiment_signups ----

// Signup records an agent's intent to participate in a platform experiment. inserted=false means
// the experiment is no longer open, or the agent was already signed up.
//
// The experiment row is locked FOR SHARE before its status is tested, which is what actually
// serializes this against StartPlatformExperimentTx: that transaction's status UPDATE takes a
// conflicting row lock, so a signup either completes before the start and is seen by it, or
// blocks until the start commits and then observes 'running' and refuses. A bare
// `WHERE EXISTS (... status='open')` predicate takes no lock at all, so it could read 'open'
// under its own snapshot and still commit after the start had chosen the participants — landing
// a durable signup with no quota row and no path to ever get one.
func (s *PlatformExperimentsStore) Signup(ctx context.Context, platformExpID, agentID string, role domain.SignupRole, quotaTier domain.QuotaTier) (bool, error) {
	tx, err := s.pool.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("platform_experiments_store.Signup: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM platform_experiments WHERE id=$1 FOR SHARE`, platformExpID).Scan(&status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("platform_experiments_store.Signup: lock experiment: %w", err)
	}
	if status != string(domain.PlatformExpOpen) {
		return false, nil
	}
	tag, err := tx.Exec(ctx, `
INSERT INTO experiment_signups (platform_experiment_id, agent_id, signed_up_at, role, quota_tier)
VALUES ($1, $2, NOW(), $3, $4)
ON CONFLICT (platform_experiment_id, agent_id) DO NOTHING`, platformExpID, agentID, string(role), string(quotaTier))
	if err != nil {
		return false, fmt.Errorf("platform_experiments_store.Signup: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("platform_experiments_store.Signup: commit: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// ListSignups returns all agent IDs signed up for a platform experiment.
func (s *PlatformExperimentsStore) ListSignups(ctx context.Context, platformExpID string) ([]string, error) {
	const q = `
SELECT agent_id FROM experiment_signups
WHERE platform_experiment_id = $1
ORDER BY signed_up_at ASC`

	rows, err := s.pool.pool.Query(ctx, q, platformExpID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.ListSignups: %w", err)
	}
	defer rows.Close()

	var agents []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("platform_experiments_store.ListSignups: scan: %w", err)
		}
		agents = append(agents, id)
	}
	return agents, rows.Err()
}

// ListSignupsByRole returns the agent IDs signed up for a platform experiment in one role.
func (s *PlatformExperimentsStore) ListSignupsByRole(ctx context.Context, platformExpID string, role domain.SignupRole) ([]string, error) {
	const q = `
SELECT agent_id FROM experiment_signups
WHERE platform_experiment_id = $1 AND role = $2
ORDER BY signed_up_at ASC`

	rows, err := s.pool.pool.Query(ctx, q, platformExpID, string(role))
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.ListSignupsByRole: %w", err)
	}
	defer rows.Close()

	var agents []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("platform_experiments_store.ListSignupsByRole: scan: %w", err)
		}
		agents = append(agents, id)
	}
	return agents, rows.Err()
}

// GetSignupRole returns the role an agent was signed up under. found=false means it is not
// signed up at all — a caller must not read that as any role.
func (s *PlatformExperimentsStore) GetSignupRole(ctx context.Context, platformExpID, agentID string) (domain.SignupRole, bool, error) {
	const q = `SELECT role FROM experiment_signups WHERE platform_experiment_id = $1 AND agent_id = $2`
	var role string
	err := s.pool.pool.QueryRow(ctx, q, platformExpID, agentID).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("platform_experiments_store.GetSignupRole: %w", err)
	}
	return domain.SignupRole(role), true, nil
}

var (
	ErrNotSignedUp              = errors.New("agent is not signed up to this platform experiment")
	ErrPlatformExperimentClosed = errors.New("platform experiment is closed")
)

// AgentDataKey returns the agent's object-store key in a platform experiment. The first call
// stores what create returns; the signup row is locked meanwhile, so a concurrent first call
// waits and reads that same key. A failed create stores nothing.
func (s *PlatformExperimentsStore) AgentDataKey(ctx context.Context, platformExpID, agentID string, create func(context.Context) (domain.DataKey, error)) (domain.DataKey, error) {
	tx, err := s.pool.pool.Begin(ctx)
	if err != nil {
		return domain.DataKey{}, fmt.Errorf("platform_experiments_store.AgentDataKey: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var key domain.DataKey
	var status string
	err = tx.QueryRow(ctx, `
SELECT s.data_access_key_id, s.data_secret_access_key, pe.status
FROM experiment_signups s JOIN platform_experiments pe ON pe.id = s.platform_experiment_id
WHERE s.platform_experiment_id = $1 AND s.agent_id = $2
FOR UPDATE OF s`, platformExpID, agentID).Scan(&key.AccessKeyID, &key.SecretAccessKey, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.DataKey{}, ErrNotSignedUp
	}
	if err != nil {
		return domain.DataKey{}, fmt.Errorf("platform_experiments_store.AgentDataKey: %w", err)
	}
	if status == string(domain.PlatformExpClosed) {
		return domain.DataKey{}, ErrPlatformExperimentClosed
	}
	if key.AccessKeyID != "" {
		return key, nil
	}
	if key, err = create(ctx); err != nil {
		return domain.DataKey{}, err
	}
	if _, err := tx.Exec(ctx, `
UPDATE experiment_signups SET data_access_key_id = $3, data_secret_access_key = $4
WHERE platform_experiment_id = $1 AND agent_id = $2`, platformExpID, agentID, key.AccessKeyID, key.SecretAccessKey); err != nil {
		return domain.DataKey{}, fmt.Errorf("platform_experiments_store.AgentDataKey: store key: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.DataKey{}, fmt.Errorf("platform_experiments_store.AgentDataKey: commit: %w", err)
	}
	return key, nil
}

// IsSignedUp returns true if the agent is signed up for the platform experiment.
func (s *PlatformExperimentsStore) IsSignedUp(ctx context.Context, platformExpID, agentID string) (bool, error) {
	const q = `
SELECT 1 FROM experiment_signups
WHERE platform_experiment_id = $1 AND agent_id = $2`

	var dummy int
	err := s.pool.pool.QueryRow(ctx, q, platformExpID, agentID).Scan(&dummy)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("platform_experiments_store.IsSignedUp: %w", err)
	}
	return true, nil
}

// CountSignups returns how many agents have signed up for a platform experiment.
func (s *PlatformExperimentsStore) CountSignups(ctx context.Context, platformExpID string) (int, error) {
	const q = `SELECT COUNT(*) FROM experiment_signups WHERE platform_experiment_id = $1`
	var n int
	if err := s.pool.pool.QueryRow(ctx, q, platformExpID).Scan(&n); err != nil {
		return 0, fmt.Errorf("platform_experiments_store.CountSignups: %w", err)
	}
	return n, nil
}

// CountSignupsByRole returns how many agents signed up for a platform experiment in one role.
func (s *PlatformExperimentsStore) CountSignupsByRole(ctx context.Context, platformExpID string, role domain.SignupRole) (int, error) {
	const q = `SELECT COUNT(*) FROM experiment_signups WHERE platform_experiment_id = $1 AND role = $2`
	var n int
	if err := s.pool.pool.QueryRow(ctx, q, platformExpID, string(role)).Scan(&n); err != nil {
		return 0, fmt.Errorf("platform_experiments_store.CountSignupsByRole: %w", err)
	}
	return n, nil
}
