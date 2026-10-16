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

// signupKeySQL is the pool entry signup s is one of: its agent type and its model, ” for the type's.
const signupKeySQL = `s.agent_type, COALESCE(s.model, '')`

// PoolBelowAtWorkError refuses a max_agents below the researchers at work of a platform experiment
// that does not run yet, or researchers that want fewer of an agent type and model at work than
// are: a change of researchers takes no researcher at work out, and a lowered max_agents takes out
// only those of a running one (evictAboveMaxTx).
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

// querier is a pool or a transaction.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// researchersAtWork counts the researchers at work of platform experiment platformExpID
// (researcherAtWorkSQL) by flavor and model.
func researchersAtWork(ctx context.Context, q querier, platformExpID string) (map[domain.PoolKey]int, error) {
	rows, err := q.Query(ctx, `
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

// ResearchersAtWork counts the researchers at work of platform experiment platformExpID by agent
// type and model.
func (s *PlatformExperimentsStore) ResearchersAtWork(ctx context.Context, platformExpID string) (map[domain.PoolKey]int, error) {
	atWork, err := researchersAtWork(ctx, s.pool.pool, platformExpID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.ResearchersAtWork: %w", err)
	}
	return atWork, nil
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

// setResearchersTx holds the platform experiment platformExpID, whose row the write locked, to the
// max_agents just written to it: a running one loses the newest of its researchers at work above
// it (evictAboveMaxTx), and its researchers, unless pool states them, are capped to it. Then pool,
// unless it is nil, is held to the researchers at work (holdsAtWork) and written.
func setResearchersTx(ctx context.Context, tx pgx.Tx, platformExpID string, maxAgents int, pool domain.ResearcherPool) error {
	if err := poolTypesTx(ctx, tx, platformExpID, pool); err != nil {
		return err
	}
	if err := evictAboveMaxTx(ctx, tx, platformExpID, maxAgents, pool == nil); err != nil {
		return err
	}
	atWork, err := researchersAtWork(ctx, tx, platformExpID)
	if err != nil {
		return err
	}
	if err := holdsAtWork(maxAgents, pool, atWork); err != nil {
		return err
	}
	if pool == nil {
		stored, err := lockedPoolTx(ctx, tx, platformExpID)
		if err != nil || stored.Total() <= maxAgents {
			return err
		}
		pool = stored.Capped(maxAgents)
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

// CutMaxAgentsLowered is why the platform cut a researcher that was one of the newest at work above
// a max_agents lowered below them (platform_experiment_cuts.reason).
const CutMaxAgentsLowered = "max_agents_lowered"

// evictAboveMaxTx cuts the newest researchers at work of the running platform experiment
// platformExpID above maxAgents, at its current stage with reason CutMaxAgentsLowered: a decision
// of the platform written as their end, after which each winds down as a cut researcher does and
// nobody takes its place. With lowerPool their entries of its researchers are lowered by them, as
// a stage cut lowers them; a change that states the researchers states them itself. A platform
// experiment that does not run evicts nobody.
func evictAboveMaxTx(ctx context.Context, tx pgx.Tx, platformExpID string, maxAgents int, lowerPool bool) error {
	var status string
	var stage int
	if err := tx.QueryRow(ctx, `SELECT status, current_stage FROM platform_experiments WHERE id = $1`, platformExpID).Scan(&status, &stage); err != nil {
		return fmt.Errorf("read status: %w", err)
	}
	if status != string(domain.PlatformExpRunning) {
		return nil
	}
	rows, err := tx.Query(ctx, `
SELECT s.agent_id FROM experiment_signups s
WHERE s.platform_experiment_id = $1 AND `+researcherAtWorkSQL+`
ORDER BY s.signed_up_at, s.agent_id
OFFSET $2`, platformExpID, maxAgents)
	if err != nil {
		return fmt.Errorf("researchers above max_agents: %w", err)
	}
	evicted, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(evicted) == 0 {
		return err
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO platform_experiment_cuts (platform_experiment_id, agent_id, stage_index, reason)
SELECT $1, unnest($2::text[]), $3, $4`, platformExpID, evicted, stage, CutMaxAgentsLowered); err != nil {
		return fmt.Errorf("cut researchers above max_agents: %w", err)
	}
	if !lowerPool {
		return nil
	}
	return lowerPoolTx(ctx, tx, platformExpID, evicted)
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
	atWork, err := researchersAtWork(ctx, tx, platformExpID)
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
// stopped with no restart left. It was not cut, and nobody replaced it yet. Early is one that
// submitted no job or ended within domain.ReplacementMinRun of joining.
type EndedResearcher struct {
	domain.AgentSignup
	Key     domain.PoolKey
	EndedAt time.Time
	Early   bool
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
	// Recent counts the researchers added in the places of others in the last hour, by agent type
	// and model.
	Recent map[domain.PoolKey]int
	// Held is what the platform last said of the places it keeps empty (HoldResearchers).
	Held string
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
	if err := tx.QueryRow(ctx, `SELECT researchers, max_agents, COALESCE(researchers_held, '') FROM platform_experiments WHERE id = $1`, platformExpID).Scan(&state.Pool, &state.MaxAgents, &state.Held); err != nil {
		return PoolState{}, fmt.Errorf("platform_experiments_store.ResearcherPoolState: read researchers: %w", err)
	}
	if state.AtWork, err = researchersAtWork(ctx, tx, platformExpID); err != nil {
		return PoolState{}, fmt.Errorf("platform_experiments_store.ResearcherPoolState: %w", err)
	}
	if state.Recent, err = recentReplacements(ctx, tx, platformExpID); err != nil {
		return PoolState{}, fmt.Errorf("platform_experiments_store.ResearcherPoolState: %w", err)
	}
	rows, err := tx.Query(ctx, `
SELECT `+signupKeySQL+`, s.agent_id, s.quota_tier, s.hyperparameters, s.added_by_kind, s.added_by_id, ended.at,
       NOT EXISTS (SELECT 1 FROM experiments e WHERE e.platform_experiment_id = s.platform_experiment_id AND e.agent_id = s.agent_id)
         OR ended.at < s.signed_up_at + make_interval(secs => $2)
FROM experiment_signups s`+signupInstanceJoin+`
CROSS JOIN LATERAL (SELECT COALESCE(s.finished_at, i.exited_at, s.signed_up_at) AS at) ended
WHERE s.platform_experiment_id = $1 AND s.role = '`+string(domain.SignupRoleCompetitor)+`'
  AND (s.finished_at IS NOT NULL OR `+stoppedSQL+`)
  AND NOT EXISTS (SELECT 1 FROM platform_experiment_cuts c WHERE c.platform_experiment_id = s.platform_experiment_id AND c.agent_id = s.agent_id)
  AND NOT `+replacedSQL+`
ORDER BY 7, s.agent_id`, platformExpID, domain.ReplacementMinRun.Seconds())
	if err != nil {
		return PoolState{}, fmt.Errorf("platform_experiments_store.ResearcherPoolState: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ended EndedResearcher
		var hyperparameters []byte
		addedBy := domain.Actor{}
		if err := rows.Scan(&ended.Key.AgentType, &ended.Key.Model, &ended.AgentID, &ended.QuotaTier, &hyperparameters, &addedBy.Kind, &addedBy.ID, &ended.EndedAt, &ended.Early); err != nil {
			return PoolState{}, fmt.Errorf("platform_experiments_store.ResearcherPoolState: scan: %w", err)
		}
		if hyperparameters != nil {
			if err := json.Unmarshal(hyperparameters, &ended.Hyperparameters); err != nil {
				return PoolState{}, fmt.Errorf("platform_experiments_store.ResearcherPoolState: decode hyperparameters of %s: %w", ended.AgentID, err)
			}
		}
		if addedBy.Kind != "" {
			ended.Actor = &addedBy
		}
		ended.PlatformExperimentID, ended.Role, ended.AgentType, ended.Model = platformExpID, domain.SignupRoleCompetitor, ended.Key.AgentType, ended.Key.Model
		state.Ended[ended.Key] = append(state.Ended[ended.Key], ended)
	}
	if err := rows.Err(); err != nil {
		return PoolState{}, fmt.Errorf("platform_experiments_store.ResearcherPoolState: %w", err)
	}
	return state, nil
}

// recentReplacements counts the researchers of platform experiment platformExpID added in the
// places of others in the last hour, by agent type and model.
func recentReplacements(ctx context.Context, q querier, platformExpID string) (map[domain.PoolKey]int, error) {
	rows, err := q.Query(ctx, `
SELECT `+signupKeySQL+`, count(*) FROM experiment_signups s
WHERE s.platform_experiment_id = $1 AND s.replaces <> '' AND s.signed_up_at > now() - interval '1 hour'
GROUP BY 1, 2`, platformExpID)
	if err != nil {
		return nil, fmt.Errorf("count recent replacements: %w", err)
	}
	recent := map[domain.PoolKey]int{}
	var key domain.PoolKey
	var n int
	_, err = pgx.ForEachRow(rows, []any{&key.AgentType, &key.Model, &n}, func() error {
		recent[key] = n
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("count recent replacements: %w", err)
	}
	return recent, nil
}

// HoldResearchers records line as what the running platform experiment id says of the places of
// its researchers the platform keeps empty, "" for none (domain.HoldsLine). A line that differs
// from the one recorded is told as platform_experiment.status with it as the detail, by the
// trigger on the row; one that is the same writes nothing.
func (s *PlatformExperimentsStore) HoldResearchers(ctx context.Context, id, line string) error {
	const q = `UPDATE platform_experiments SET researchers_held = NULLIF($2, '')
WHERE id = $1 AND status = 'running' AND researchers_held IS DISTINCT FROM NULLIF($2, '')`
	if _, err := s.pool.pool.Exec(ctx, q, id, line); err != nil {
		return fmt.Errorf("platform_experiments_store.HoldResearchers: %w", err)
	}
	return nil
}

// ErrPoolBelowAtWork is a pool refused for wanting fewer researchers at work than are
// (PoolBelowAtWorkError).
var ErrPoolBelowAtWork = errors.New("researchers below the researchers at work")

func (e *PoolBelowAtWorkError) Is(target error) bool { return target == ErrPoolBelowAtWork }
