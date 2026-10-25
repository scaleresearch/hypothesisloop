package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/scaleresearch/hypothesisloop/contract/platformapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// signupKeySQL is the pool entry signup s is one of: its agent type and its model, ” for the type's.
const signupKeySQL = `s.agent_type, COALESCE(s.model, '')`

// PoolBelowAtWorkError refuses a max_agents below the executors at work of a platform experiment
// that does not run yet, or agents that want fewer of an agent type and model at work than are: a
// change of agents takes no agent at work out, and a lowered max_agents takes out only executors
// of a running one (evictAboveMaxTx).
type PoolBelowAtWorkError struct {
	// Key is the entry's agent type and model, empty for max_agents.
	Key    domain.PoolKey
	Count  int
	AtWork int
}

func (e *PoolBelowAtWorkError) Error() string {
	if e.Key == (domain.PoolKey{}) {
		return fmt.Sprintf("max_agents %d is below the %d executors at work", e.Count, e.AtWork)
	}
	return fmt.Sprintf("agents want %d of agent_type %s on model %s, and %d of them are at work", e.Count, e.Key.AgentType, e.Key.Model, e.AtWork)
}

// poolJSON is pool as the agents column holds it, an empty list for none.
func poolJSON(pool domain.AgentPool) ([]byte, error) {
	if pool == nil {
		pool = domain.AgentPool{}
	}
	return json.Marshal(pool)
}

// agentsAtWork counts the agents at work of platform experiment platformExpID (agentAtWorkSQL),
// executors and researchers, by agent type and model.
func agentsAtWork(ctx context.Context, q queryer, platformExpID string) (map[domain.PoolKey]int, error) {
	rows, err := q.Query(ctx, `
SELECT `+signupKeySQL+`, count(*) FROM experiment_signups s
WHERE s.platform_experiment_id = $1 AND `+agentAtWorkSQL+`
GROUP BY 1, 2`, platformExpID)
	if err != nil {
		return nil, fmt.Errorf("count agents at work: %w", err)
	}
	defer rows.Close()
	atWork := map[domain.PoolKey]int{}
	for rows.Next() {
		var key domain.PoolKey
		var n int
		if err := rows.Scan(&key.AgentType, &key.Model, &n); err != nil {
			return nil, fmt.Errorf("count agents at work: %w", err)
		}
		atWork[key] = n
	}
	return atWork, rows.Err()
}

// AgentsAtWork counts the agents at work of platform experiment platformExpID by agent type and
// model.
func (s *Store) AgentsAtWork(ctx context.Context, platformExpID string) (map[domain.PoolKey]int, error) {
	atWork, err := agentsAtWork(ctx, s.pool.pool, platformExpID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.AgentsAtWork: %w", err)
	}
	return atWork, nil
}

// holdsAtWork refuses a max_agents below the executors at work, and a pool, unless nil, that
// wants fewer of an entry at work than are.
func holdsAtWork(maxAgents int, pool domain.AgentPool, atWork map[domain.PoolKey]int) error {
	working := 0
	for key, n := range atWork {
		if !key.Researches() {
			working += n
		}
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

// setAgentsTx holds the platform experiment platformExpID, whose row the write locked, to the
// max_agents just written to it: a running one loses the newest of its executors at work above
// it (evictAboveMaxTx), and its agents, unless pool states them, are capped to it. Then pool,
// unless it is nil, is held to the agents at work (holdsAtWork) and written. It answers
// whether the agents it wrote differ from those stored.
func setAgentsTx(ctx context.Context, tx *Write, platformExpID string, maxAgents int, pool domain.AgentPool) (bool, error) {
	if err := poolTypesTx(ctx, tx, platformExpID, pool); err != nil {
		return false, err
	}
	if err := evictAboveMaxTx(ctx, tx, platformExpID, maxAgents, pool == nil); err != nil {
		return false, err
	}
	if err := evictAboveEntriesTx(ctx, tx, platformExpID, pool); err != nil {
		return false, err
	}
	atWork, err := agentsAtWork(ctx, tx, platformExpID)
	if err != nil {
		return false, err
	}
	if err := holdsAtWork(maxAgents, pool, atWork); err != nil {
		return false, err
	}
	if pool == nil {
		stored, err := lockedPoolTx(ctx, tx, platformExpID)
		if err != nil || stored.Executors() <= maxAgents {
			return false, err
		}
		pool = stored.Capped(maxAgents)
	}
	encoded, err := poolJSON(pool)
	if err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx, `UPDATE platform_experiments SET agents = $2 WHERE id = $1 AND agents IS DISTINCT FROM $2`, platformExpID, encoded)
	if err != nil {
		return false, fmt.Errorf("write agents: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// CutMaxAgentsLowered is why the platform cut an executor that was one of the newest at work above
// a max_agents lowered below them (platform_experiment_cuts.reason).
const CutMaxAgentsLowered = "max_agents_lowered"

// evictAboveMaxTx cuts the newest executors at work of the running platform experiment
// platformExpID above maxAgents, at its current stage with reason CutMaxAgentsLowered: a decision
// of the platform written as their end, after which each winds down as a cut executor does and
// nobody takes its place. With lowerPool their entries of its agents are lowered by them, as
// a stage cut lowers them; a change that states the agents states them itself. A platform
// experiment that does not run evicts nobody.
func evictAboveMaxTx(ctx context.Context, tx *Write, platformExpID string, maxAgents int, lowerPool bool) error {
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
WHERE s.platform_experiment_id = $1 AND `+executorAtWorkSQL+`
ORDER BY s.signed_up_at, s.agent_id
OFFSET $2`, platformExpID, maxAgents)
	if err != nil {
		return fmt.Errorf("executors above max_agents: %w", err)
	}
	evicted, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(evicted) == 0 {
		return err
	}
	if err := cutTx(ctx, tx, platformExpID, evicted, stage, CutMaxAgentsLowered); err != nil {
		return fmt.Errorf("cut executors above max_agents: %w", err)
	}
	if !lowerPool {
		return nil
	}
	return lowerPoolTx(ctx, tx, platformExpID, evicted)
}

// evictAboveEntriesTx cuts, of the running platform experiment platformExpID, the newest executors
// at work of each agent type and model above the count pool wants of it, none for one pool does
// not name, as a lowered max_agents cuts them. A pool that is nil, and one that does not run,
// cut nobody. Researchers are never cut: holdsAtWork refuses fewer of them than are at work.
func evictAboveEntriesTx(ctx context.Context, tx *Write, platformExpID string, pool domain.AgentPool) error {
	if pool == nil {
		return nil
	}
	var status string
	var stage int
	if err := tx.QueryRow(ctx, `SELECT status, current_stage FROM platform_experiments WHERE id = $1`, platformExpID).Scan(&status, &stage); err != nil {
		return fmt.Errorf("read status: %w", err)
	}
	if status != string(domain.PlatformExpRunning) {
		return nil
	}
	rows, err := tx.Query(ctx, `
SELECT s.agent_id, `+signupKeySQL+` FROM experiment_signups s
WHERE s.platform_experiment_id = $1 AND `+executorAtWorkSQL+`
ORDER BY s.signed_up_at, s.agent_id`, platformExpID)
	if err != nil {
		return fmt.Errorf("executors at work: %w", err)
	}
	type working struct {
		agentID string
		key     domain.PoolKey
	}
	all, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (working, error) {
		var w working
		return w, row.Scan(&w.agentID, &w.key.AgentType, &w.key.Model)
	})
	if err != nil {
		return fmt.Errorf("executors at work: %w", err)
	}
	wanted := map[domain.PoolKey]int{}
	for _, e := range pool {
		wanted[e.Key()] += e.Count
	}
	seen := map[domain.PoolKey]int{}
	var evicted []string
	for _, w := range all {
		if seen[w.key]++; seen[w.key] > wanted[w.key] {
			evicted = append(evicted, w.agentID)
		}
	}
	if len(evicted) == 0 {
		return nil
	}
	if err := cutTx(ctx, tx, platformExpID, evicted, stage, CutMaxAgentsLowered); err != nil {
		return fmt.Errorf("cut executors above agents: %w", err)
	}
	return nil
}

// lockedPoolTx locks platform experiment platformExpID and reads its agents.
func lockedPoolTx(ctx context.Context, tx *Write, platformExpID string) (domain.AgentPool, error) {
	var pool domain.AgentPool
	if err := tx.QueryRow(ctx, `SELECT agents FROM platform_experiments WHERE id = $1 FOR UPDATE`, platformExpID).Scan(&pool); err != nil {
		return nil, fmt.Errorf("read agents: %w", err)
	}
	return pool, nil
}

// signupKeysTx is the agent type and model of each of agentIDs that is an executor or a
// researcher of platform experiment platformExpID, the roles its agents keep (teamSQL).
func signupKeysTx(ctx context.Context, tx *Write, platformExpID string, agentIDs []string) ([]domain.PoolKey, error) {
	rows, err := tx.Query(ctx, `
SELECT `+signupKeySQL+` FROM experiment_signups s
WHERE s.platform_experiment_id = $1 AND s.agent_id = ANY($2) AND `+teamSQL+`
ORDER BY s.agent_id`, platformExpID, agentIDs)
	if err != nil {
		return nil, fmt.Errorf("read agent types: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.PoolKey, error) {
		var key domain.PoolKey
		err := row.Scan(&key.AgentType, &key.Model)
		return key, err
	})
}

// lowerPoolTx lowers the agents of platform experiment platformExpID for the executors of
// agentIDs it cut, who leave it for good: nobody takes their places. Each lowers its
// entry by one while that entry wants more at work than are, so one the coordinator added beyond
// the entry lowers nothing.
func lowerPoolTx(ctx context.Context, tx *Write, platformExpID string, agentIDs []string) error {
	keys, err := signupKeysTx(ctx, tx, platformExpID, agentIDs)
	if err != nil {
		return err
	}
	for _, key := range keys {
		if err := shrinkPoolToAtWorkTx(ctx, tx, platformExpID, key); err != nil {
			return err
		}
	}
	return nil
}

// shrinkPoolToAtWorkTx lowers the entry of key in the agents of the locked platform
// experiment platformExpID by one when it wants more of them at work than are.
func shrinkPoolToAtWorkTx(ctx context.Context, tx *Write, platformExpID string, key domain.PoolKey) error {
	pool, err := lockedPoolTx(ctx, tx, platformExpID)
	if err != nil {
		return err
	}
	atWork, err := agentsAtWork(ctx, tx, platformExpID)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(pool, func(e domain.AgentPoolEntry) bool { return e.Key() == key })
	if i < 0 || pool[i].Count <= atWork[key] {
		return nil
	}
	pool[i].Count--
	encoded, err := poolJSON(pool)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE platform_experiments SET agents = $2 WHERE id = $1`, platformExpID, encoded); err != nil {
		return fmt.Errorf("lower agents: %w", err)
	}
	return nil
}

// EndedAgent is an agent of the team whose place is free for good: it finished, or its
// machine stopped with no restart left. It was not cut, and nobody replaced it yet. Early is one
// that ended within domain.ReplacementMinRun of joining or, an executor, submitted no job.
type EndedAgent struct {
	domain.AgentSignup
	Key     domain.PoolKey
	EndedAt time.Time
	Early   bool
}

// PoolState is what keeping the agents of a running platform experiment is decided from.
type PoolState struct {
	// Pool is the agents it is to have at work, read with the rest: a cut since the platform
	// experiment was read has lowered it.
	Pool domain.AgentPool
	// AtWork counts the agents at work by agent type and model (agentAtWorkSQL).
	AtWork map[domain.PoolKey]int
	// MaxAgents is the cap on the executors at work, read with the rest.
	MaxAgents int
	// Ended is every agent whose place is free for good, by agent type and model, the one that
	// ended first first: an executor or a researcher that finished or whose machine stopped with
	// no restart left.
	Ended map[domain.PoolKey][]EndedAgent
	// Recent counts the agents added in the places of others in the last hour, by agent type
	// and model.
	Recent map[domain.PoolKey]int
	// Held is what the platform last said of the places it keeps empty (HoldAgents).
	Held string
}

// AgentPoolState reads what keeping the agents of platform experiment platformExpID is
// decided from.
func (s *Store) AgentPoolState(ctx context.Context, platformExpID string) (PoolState, error) {
	tx, err := s.pool.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return PoolState{}, fmt.Errorf("platform_experiments_store.AgentPoolState: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	state := PoolState{Ended: map[domain.PoolKey][]EndedAgent{}}
	if err := tx.QueryRow(ctx, `SELECT agents, max_agents, COALESCE(agents_held, '') FROM platform_experiments WHERE id = $1`, platformExpID).Scan(&state.Pool, &state.MaxAgents, &state.Held); err != nil {
		return PoolState{}, fmt.Errorf("platform_experiments_store.AgentPoolState: read agents: %w", err)
	}
	if state.AtWork, err = agentsAtWork(ctx, tx, platformExpID); err != nil {
		return PoolState{}, fmt.Errorf("platform_experiments_store.AgentPoolState: %w", err)
	}
	if state.Recent, err = recentReplacements(ctx, tx, platformExpID); err != nil {
		return PoolState{}, fmt.Errorf("platform_experiments_store.AgentPoolState: %w", err)
	}
	rows, err := tx.Query(ctx, `
SELECT `+signupKeySQL+`, s.role, s.agent_id, s.quota_tier, s.hyperparameters, s.added_by_kind, s.added_by_id, ended.at,
       s.role <> '`+string(domain.SignupRoleResearcher)+`'
         AND NOT EXISTS (SELECT 1 FROM experiments e WHERE e.platform_experiment_id = s.platform_experiment_id AND e.agent_id = s.agent_id)
         OR ended.at < s.signed_up_at + make_interval(secs => $2)
FROM experiment_signups s`+signupInstanceJoin+`
CROSS JOIN LATERAL (SELECT COALESCE(s.finished_at, i.exited_at, s.signed_up_at) AS at) ended
WHERE s.platform_experiment_id = $1 AND `+teamSQL+`
  AND (s.finished_at IS NOT NULL OR `+stoppedSQL+`)
  AND NOT EXISTS (SELECT 1 FROM platform_experiment_cuts c WHERE c.platform_experiment_id = s.platform_experiment_id AND c.agent_id = s.agent_id)
  AND NOT `+replacedSQL+`
ORDER BY 8, s.agent_id`, platformExpID, domain.ReplacementMinRun.Seconds())
	if err != nil {
		return PoolState{}, fmt.Errorf("platform_experiments_store.AgentPoolState: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ended EndedAgent
		var hyperparameters []byte
		addedBy := domain.Actor{}
		if err := rows.Scan(&ended.Key.AgentType, &ended.Key.Model, &ended.Role, &ended.AgentID, &ended.QuotaTier, &hyperparameters, &addedBy.Kind, &addedBy.ID, &ended.EndedAt, &ended.Early); err != nil {
			return PoolState{}, fmt.Errorf("platform_experiments_store.AgentPoolState: scan: %w", err)
		}
		if hyperparameters != nil {
			if err := json.Unmarshal(hyperparameters, &ended.Hyperparameters); err != nil {
				return PoolState{}, fmt.Errorf("platform_experiments_store.AgentPoolState: decode hyperparameters of %s: %w", ended.AgentID, err)
			}
		}
		if addedBy.Kind != "" {
			ended.Actor = &addedBy
		}
		ended.PlatformExperimentID, ended.AgentType, ended.Model = platformExpID, ended.Key.AgentType, ended.Key.Model
		state.Ended[ended.Key] = append(state.Ended[ended.Key], ended)
	}
	if err := rows.Err(); err != nil {
		return PoolState{}, fmt.Errorf("platform_experiments_store.AgentPoolState: %w", err)
	}
	return state, nil
}

// recentReplacements counts the agents of platform experiment platformExpID added in the
// places of others in the last hour, by agent type and model.
func recentReplacements(ctx context.Context, q queryer, platformExpID string) (map[domain.PoolKey]int, error) {
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

// HoldAgents records line as what the running platform experiment id says of the places of
// its agents the platform keeps empty, "" for none (domain.HoldsLine). A line that differs
// from the one recorded is told as platform_experiment.status with it as the detail; one that is
// the same writes nothing.
func (s *Store) HoldAgents(ctx context.Context, id, line string) error {
	const q = `UPDATE platform_experiments SET agents_held = NULLIF($2, '')
WHERE id = $1 AND status = 'running' AND agents_held IS DISTINCT FROM NULLIF($2, '')`
	if err := s.writeRunStatusDetail(ctx, q, id, line); err != nil {
		return fmt.Errorf("platform_experiments_store.HoldAgents: %w", err)
	}
	return nil
}

// ErrPoolBelowAtWork is a pool refused for wanting fewer agents at work than are
// (PoolBelowAtWorkError).
var ErrPoolBelowAtWork = errors.New("agents below the agents at work")

func (e *PoolBelowAtWorkError) Is(target error) bool { return target == ErrPoolBelowAtWork }

// cutTx cuts agentIDs at stage for reason, ” for a stage cut, each agent once, and announces each
// cut: to the agent, and a stage cut also as a move of the ladder.
func cutTx(ctx context.Context, tx *Write, platformExpID string, agentIDs []string, stage int, reason string) error {
	rows, err := tx.Query(ctx, `
INSERT INTO platform_experiment_cuts (platform_experiment_id, agent_id, stage_index, reason)
SELECT $1, unnest($2::text[]), $3, $4 ON CONFLICT DO NOTHING
RETURNING agent_id`, platformExpID, agentIDs, stage, reason)
	if err != nil {
		return fmt.Errorf("cut %v: %w", agentIDs, err)
	}
	cut, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("cut %v: %w", agentIDs, err)
	}
	for _, agentID := range cut {
		if reason == "" {
			tx.Change(domain.Change{Kind: platformapi.EventStageBoundary, Subject: platformExpID, Value: strconv.Itoa(stage), Detail: "cut",
				PlatformExperimentID: platformExpID, AgentID: agentID})
		}
		tx.Change(domain.Change{Kind: platformapi.EventAgentCut, Subject: agentID, Value: strconv.Itoa(stage), Detail: reason,
			PlatformExperimentID: platformExpID, AgentID: agentID})
	}
	return nil
}
