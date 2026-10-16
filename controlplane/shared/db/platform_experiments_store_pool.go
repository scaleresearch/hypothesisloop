package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// signupKeySQL is the pool entry signup s is one of: its agent type, which the signup holds as
// its flavor, and its model. A flavor from before flavors were stored reads as the default.
const signupKeySQL = `COALESCE(NULLIF(s.flavor, ''), '` + domain.DefaultAgentFlavor + `'), s.model`

// PoolBelowAtWorkError refuses a max_agents below the researchers at work, or researchers that
// want fewer of an agent type and model at work than are: a researcher at work is never taken out
// by a change of either.
type PoolBelowAtWorkError struct {
	// Key is the entry's agent type and model, empty for max_agents.
	Key    domain.PoolKey
	Count  int
	AtWork int
}

func (e *PoolBelowAtWorkError) Error() string {
	if e.Key == (domain.PoolKey{}) {
		return fmt.Sprintf("max_agents %d is below the %d researchers at work", e.Count, e.AtWork)
	}
	return fmt.Sprintf("researchers want %d of agent_type %s on model %s, and %d of them are at work", e.Count, e.Key.AgentType, e.Key.Model, e.AtWork)
}

// poolJSON is pool as the researchers column holds it, an empty list for none.
func poolJSON(pool domain.ResearcherPool) ([]byte, error) {
	if pool == nil {
		pool = domain.ResearcherPool{}
	}
	return json.Marshal(pool)
}

// researchersAtWorkTx counts the researchers at work of platform experiment platformExpID
// (researcherAtWorkSQL) by flavor and model.
func researchersAtWorkTx(ctx context.Context, tx pgx.Tx, platformExpID string) (map[domain.PoolKey]int, error) {
	rows, err := tx.Query(ctx, `
SELECT `+signupKeySQL+`, count(*) FROM experiment_signups s
WHERE s.platform_experiment_id = $1 AND `+researcherAtWorkSQL+`
GROUP BY 1, 2`, platformExpID)
	if err != nil {
		return nil, fmt.Errorf("count researchers at work: %w", err)
	}
	defer rows.Close()
	atWork := map[domain.PoolKey]int{}
	for rows.Next() {
		var key domain.PoolKey
		var n int
		if err := rows.Scan(&key.AgentType, &key.Model, &n); err != nil {
			return nil, fmt.Errorf("count researchers at work: %w", err)
		}
		atWork[key] = n
	}
	return atWork, rows.Err()
}

// holdsAtWork refuses a max_agents below the researchers at work, and a pool, unless nil, that
// wants fewer of an entry at work than are.
func holdsAtWork(maxAgents int, pool domain.ResearcherPool, atWork map[domain.PoolKey]int) error {
	working := 0
	for _, n := range atWork {
		working += n
	}
	if maxAgents < working {
		return &PoolBelowAtWorkError{Count: maxAgents, AtWork: working}
	}
	for _, e := range pool {
		if n := atWork[e.Key()]; e.Count < n {
			return &PoolBelowAtWorkError{Key: e.Key(), Count: e.Count, AtWork: n}
		}
	}
	return nil
}

// setResearchersTx holds the max_agents just written to the platform experiment platformExpID,
// whose row the write locked, and pool unless it is nil, to the researchers at work
// (holdsAtWork), and writes pool unless it is nil.
func setResearchersTx(ctx context.Context, tx pgx.Tx, platformExpID string, maxAgents int, pool domain.ResearcherPool) error {
	atWork, err := researchersAtWorkTx(ctx, tx, platformExpID)
	if err != nil {
		return err
	}
	if err := holdsAtWork(maxAgents, pool, atWork); err != nil {
		return err
	}
	if pool == nil {
		return nil
	}
	encoded, err := poolJSON(pool)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE platform_experiments SET researchers = $2 WHERE id = $1`, platformExpID, encoded); err != nil {
		return fmt.Errorf("write researchers: %w", err)
	}
	return nil
}

// lockedPoolTx locks platform experiment platformExpID and reads its researchers.
func lockedPoolTx(ctx context.Context, tx pgx.Tx, platformExpID string) (domain.ResearcherPool, error) {
	var pool domain.ResearcherPool
	if err := tx.QueryRow(ctx, `SELECT researchers FROM platform_experiments WHERE id = $1 FOR UPDATE`, platformExpID).Scan(&pool); err != nil {
		return nil, fmt.Errorf("read researchers: %w", err)
	}
	return pool, nil
}

// signupKeysTx is the agent type and model of each of agentIDs that is a researcher of platform
// experiment platformExpID.
func signupKeysTx(ctx context.Context, tx pgx.Tx, platformExpID string, agentIDs []string) ([]domain.PoolKey, error) {
	rows, err := tx.Query(ctx, `
SELECT `+signupKeySQL+` FROM experiment_signups s
WHERE s.platform_experiment_id = $1 AND s.agent_id = ANY($2) AND s.role = '`+string(domain.SignupRoleCompetitor)+`'
ORDER BY s.agent_id`, platformExpID, agentIDs)
	if err != nil {
		return nil, fmt.Errorf("read researchers' flavors: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.PoolKey, error) {
		var key domain.PoolKey
		err := row.Scan(&key.AgentType, &key.Model)
		return key, err
	})
}

// lowerPoolTx writes the researchers of platform experiment platformExpID less the researchers
// of agentIDs, who leave it for good (domain.ResearcherPool.Lowered): nobody takes their places.
func lowerPoolTx(ctx context.Context, tx pgx.Tx, platformExpID string, agentIDs []string) error {
	pool, err := lockedPoolTx(ctx, tx, platformExpID)
	if err != nil {
		return err
	}
	keys, err := signupKeysTx(ctx, tx, platformExpID, agentIDs)
	if err != nil || len(keys) == 0 {
		return err
	}
	encoded, err := poolJSON(pool.Lowered(keys))
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE platform_experiments SET researchers = $2 WHERE id = $1`, platformExpID, encoded); err != nil {
		return fmt.Errorf("lower researchers: %w", err)
	}
	return nil
}

// shrinkPoolToAtWorkTx lowers the entry of key in the researchers of the locked platform
// experiment platformExpID by one when it wants more of them at work than are.
func shrinkPoolToAtWorkTx(ctx context.Context, tx pgx.Tx, platformExpID string, key domain.PoolKey) error {
	pool, err := lockedPoolTx(ctx, tx, platformExpID)
	if err != nil {
		return err
	}
	atWork, err := researchersAtWorkTx(ctx, tx, platformExpID)
	if err != nil {
		return err
	}
	for i, e := range pool {
		if e.Key() == key && e.Count > atWork[key] {
			pool[i].Count--
			encoded, err := poolJSON(pool)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE platform_experiments SET researchers = $2 WHERE id = $1`, platformExpID, encoded); err != nil {
				return fmt.Errorf("lower researchers: %w", err)
			}
			return nil
		}
	}
	return nil
}

// EndedResearcher is a researcher whose place is free for good: it finished, or its machine
// stopped with no restart left. It was not cut, and nobody replaced it yet.
type EndedResearcher struct {
	domain.AgentSignup
	Key     domain.PoolKey
	EndedAt time.Time
}

// PoolState is what keeping the researchers of a running platform experiment is decided from.
type PoolState struct {
	// Pool is the researchers it is to have at work, read with the rest: a cut since the platform
	// experiment was read has lowered it.
	Pool domain.ResearcherPool
	// AtWork counts the researchers at work by agent type and model (researcherAtWorkSQL).
	AtWork map[domain.PoolKey]int
	// MaxAgents is the cap on the researchers at work, read with the rest.
	MaxAgents int
	// Ended is every researcher whose place is free for good, by agent type and model, the one that
	// ended first first.
	Ended map[domain.PoolKey][]EndedResearcher
}

// ResearcherPoolState reads what keeping the researchers of platform experiment platformExpID is
// decided from.
func (s *PlatformExperimentsStore) ResearcherPoolState(ctx context.Context, platformExpID string) (PoolState, error) {
	tx, err := s.pool.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return PoolState{}, fmt.Errorf("platform_experiments_store.ResearcherPoolState: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	state := PoolState{Ended: map[domain.PoolKey][]EndedResearcher{}}
	if err := tx.QueryRow(ctx, `SELECT researchers, max_agents FROM platform_experiments WHERE id = $1`, platformExpID).Scan(&state.Pool, &state.MaxAgents); err != nil {
		return PoolState{}, fmt.Errorf("platform_experiments_store.ResearcherPoolState: read researchers: %w", err)
	}
	if state.AtWork, err = researchersAtWorkTx(ctx, tx, platformExpID); err != nil {
		return PoolState{}, fmt.Errorf("platform_experiments_store.ResearcherPoolState: %w", err)
	}
	rows, err := tx.Query(ctx, `
SELECT `+signupKeySQL+`, s.agent_id, s.quota_tier, s.hyperparameters, s.added_by_kind, s.added_by_id,
       COALESCE(s.finished_at, s.instance_exited_at, s.signed_up_at)
FROM experiment_signups s
WHERE s.platform_experiment_id = $1 AND s.role = '`+string(domain.SignupRoleCompetitor)+`'
  AND (s.finished_at IS NOT NULL OR (s.hosted AND s.instance_stopped))
  AND NOT EXISTS (SELECT 1 FROM platform_experiment_cuts c WHERE c.platform_experiment_id = s.platform_experiment_id AND c.agent_id = s.agent_id)
  AND NOT `+replacedSQL+`
ORDER BY 7, s.agent_id`, platformExpID)
	if err != nil {
		return PoolState{}, fmt.Errorf("platform_experiments_store.ResearcherPoolState: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ended EndedResearcher
		var hyperparameters []byte
		addedBy := domain.Actor{}
		if err := rows.Scan(&ended.Key.AgentType, &ended.Key.Model, &ended.AgentID, &ended.QuotaTier, &hyperparameters, &addedBy.Kind, &addedBy.ID, &ended.EndedAt); err != nil {
			return PoolState{}, fmt.Errorf("platform_experiments_store.ResearcherPoolState: scan: %w", err)
		}
		if err := json.Unmarshal(hyperparameters, &ended.Hyperparameters); err != nil {
			return PoolState{}, fmt.Errorf("platform_experiments_store.ResearcherPoolState: decode hyperparameters of %s: %w", ended.AgentID, err)
		}
		if addedBy.Kind != "" {
			ended.Actor = &addedBy
		}
		ended.PlatformExperimentID, ended.Role, ended.Flavor, ended.Model = platformExpID, domain.SignupRoleCompetitor, ended.Key.AgentType, ended.Key.Model
		state.Ended[ended.Key] = append(state.Ended[ended.Key], ended)
	}
	if err := rows.Err(); err != nil {
		return PoolState{}, fmt.Errorf("platform_experiments_store.ResearcherPoolState: %w", err)
	}
	return state, nil
}

// ErrPoolBelowAtWork is a pool refused for wanting fewer researchers at work than are
// (PoolBelowAtWorkError).
var ErrPoolBelowAtWork = errors.New("researchers below the researchers at work")

func (e *PoolBelowAtWorkError) Is(target error) bool { return target == ErrPoolBelowAtWork }
