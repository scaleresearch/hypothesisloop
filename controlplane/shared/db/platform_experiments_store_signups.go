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

// ---- experiment_signups ----

// ErrMaxAgentsReached is a competitor signup refused because the field is full.
var ErrMaxAgentsReached = errors.New("max_agents reached")

// ErrMaxHostedReached is returned when a platform experiment would have the platform run more
// machines than its max_hosted.
var ErrMaxHostedReached = errors.New("max_hosted reached")

// ErrSignupClosed is a signup or a removal refused because the platform experiment is no longer
// open, or does not exist: the roster is fixed when the experiment starts.
var ErrSignupClosed = errors.New("platform experiment is not open")

// ErrAgentOutsideOrg is a signup refused because the agent is not one of the platform
// experiment's org, or does not exist.
var ErrAgentOutsideOrg = errors.New("agent is not of the platform experiment's org")

// Signup records an existing agent's place in a platform experiment. inserted=false means the
// experiment is no longer open, or the agent was already signed up.
//
// The experiment row is locked FOR UPDATE before its status is tested. That serializes this
// against StartPlatformExperimentTx, whose status UPDATE takes the same row lock: a signup either
// completes before the start and is seen by it, or blocks until the start commits and then
// observes 'running' and refuses. It also serializes signups against each other, which is what
// makes the max_agents count below exact: a count taken outside this lock let concurrent
// competitors each see room and all commit.
func (s *PlatformExperimentsStore) Signup(ctx context.Context, platformExpID string, signup domain.AgentSignup) (bool, error) {
	inserted, err := s.enrol(ctx, platformExpID, nil, []domain.AgentSignup{signup})
	if errors.Is(err, ErrSignupClosed) {
		return false, nil
	}
	return inserted == 1, err
}

// SpawnAgent creates an agent in the platform experiment's org and signs it up, both or neither.
// ErrSignupClosed when the experiment is not open, ErrMaxAgentsReached when it is a competitor
// with no place left, ErrMaxHostedReached when it is one machine too many.
func (s *PlatformExperimentsStore) SpawnAgent(ctx context.Context, platformExpID string, agent *domain.Agent, signup domain.AgentSignup) error {
	_, err := s.enrol(ctx, platformExpID, []*domain.Agent{agent}, []domain.AgentSignup{signup})
	return err
}

// enrol is the one transaction a signup row is written in. create is inserted first, into the org
// the platform experiment belongs to.
func (s *PlatformExperimentsStore) enrol(ctx context.Context, platformExpID string, create []*domain.Agent, signups []domain.AgentSignup) (int, error) {
	tx, err := s.pool.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("platform_experiments_store.Signup: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	inserted, err := enrolTx(ctx, tx, platformExpID, create, signups)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("platform_experiments_store.Signup: commit: %w", err)
	}
	return inserted, nil
}

// enrolTx is enrol inside the caller's transaction.
func enrolTx(ctx context.Context, tx pgx.Tx, platformExpID string, create []*domain.Agent, signups []domain.AgentSignup) (int, error) {
	var status, orgID string
	var maxAgents, maxHosted int
	var coordinatorHosted bool
	if err := tx.QueryRow(ctx, `
SELECT pe.status, pe.max_agents, pe.max_hosted, pe.coordinator_hosted, p.org_id
FROM platform_experiments pe JOIN projects p ON p.id = pe.project_id
WHERE pe.id=$1 FOR UPDATE OF pe`, platformExpID).Scan(&status, &maxAgents, &maxHosted, &coordinatorHosted, &orgID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, ErrSignupClosed
		}
		return 0, fmt.Errorf("platform_experiments_store.Signup: lock experiment: %w", err)
	}
	if status != string(domain.PlatformExpOpen) {
		return 0, ErrSignupClosed
	}
	for _, agent := range create {
		agent.OrgID = orgID
		if _, err := tx.Exec(ctx, `
INSERT INTO agents (id, name, org_id, performance_score, created_at)
VALUES ($1, $2, $3, $4, $5)`, agent.ID, agent.Name, agent.OrgID, agent.PerformanceScore, agent.CreatedAt); err != nil {
			return 0, fmt.Errorf("platform_experiments_store.Signup: create agent %s: %w", agent.ID, err)
		}
	}
	created := make(map[string]bool, len(create))
	for _, agent := range create {
		created[agent.ID] = true
	}
	inserted, competitors, hosted := 0, false, false
	for _, signup := range signups {
		if !created[signup.AgentID] {
			var agentOrg string
			err := tx.QueryRow(ctx, `SELECT org_id FROM agents WHERE id=$1`, signup.AgentID).Scan(&agentOrg)
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && agentOrg != orgID) {
				return 0, ErrAgentOutsideOrg
			}
			if err != nil {
				return 0, fmt.Errorf("platform_experiments_store.Signup: read agent %s: %w", signup.AgentID, err)
			}
		}
		hyperparameters, err := json.Marshal(nonNilObject(signup.Hyperparameters))
		if err != nil {
			return 0, fmt.Errorf("platform_experiments_store.Signup: encode hyperparameters: %w", err)
		}
		tag, err := tx.Exec(ctx, `
INSERT INTO experiment_signups (platform_experiment_id, agent_id, signed_up_at, role, quota_tier, hosted, launcher, flavor, hyperparameters, model)
VALUES ($1, $2, NOW(), $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (platform_experiment_id, agent_id) DO NOTHING`, platformExpID, signup.AgentID, string(signup.Role), string(signup.QuotaTier),
			signup.Hosted, signup.Launcher, signup.Flavor, hyperparameters, signup.Model)
		if err != nil {
			return 0, fmt.Errorf("platform_experiments_store.Signup: %w", err)
		}
		if tag.RowsAffected() == 1 {
			inserted++
			competitors = competitors || signup.Role == domain.SignupRoleCompetitor
			hosted = hosted || signup.Hosted
		}
	}
	// max_hosted bounds the machines the platform rents for the experiment, so it counts every
	// hosted agent whatever its role, and the coordinator.
	if hosted || (len(signups) == 0 && coordinatorHosted) {
		var count int
		// A planner's machine is gone by the time anyone can join: it is no machine of the run.
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM experiment_signups WHERE platform_experiment_id = $1 AND hosted AND flavor <> '`+domain.PlannerFlavor+`'`, platformExpID).Scan(&count); err != nil {
			return 0, fmt.Errorf("platform_experiments_store.Signup: count hosted: %w", err)
		}
		if coordinatorHosted {
			count++
		}
		if count > maxHosted {
			return 0, ErrMaxHostedReached
		}
	}
	if inserted == 0 {
		return 0, nil
	}
	// max_agents sizes the field being ranked, so it counts competitors only.
	if competitors {
		var count int
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM experiment_signups WHERE platform_experiment_id = $1 AND role = $2`,
			platformExpID, string(domain.SignupRoleCompetitor)).Scan(&count); err != nil {
			return 0, fmt.Errorf("platform_experiments_store.Signup: count competitors: %w", err)
		}
		if count > maxAgents {
			return 0, ErrMaxAgentsReached
		}
	}
	return inserted, nil
}

func nonNilObject(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// RemoveAgent takes an agent out of a platform experiment that has not started. ErrSignupClosed
// once it has, ErrNotSignedUp when the agent is not in it. Only its place goes: the agent itself
// stays in its org however it came to be, as every agent that ever joined does, and can be added
// again by its id. The experiment row is locked as in
// enrol, so a removal and a start cannot both believe they came first.
func (s *PlatformExperimentsStore) RemoveAgent(ctx context.Context, platformExpID, agentID string) error {
	tx, err := s.pool.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("platform_experiments_store.RemoveAgent: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM platform_experiments WHERE id=$1 FOR UPDATE`, platformExpID).Scan(&status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrSignupClosed
		}
		return fmt.Errorf("platform_experiments_store.RemoveAgent: lock experiment: %w", err)
	}
	if status != string(domain.PlatformExpOpen) {
		return ErrSignupClosed
	}
	tag, err := tx.Exec(ctx, `DELETE FROM experiment_signups WHERE platform_experiment_id=$1 AND agent_id=$2`, platformExpID, agentID)
	if err != nil {
		return fmt.Errorf("platform_experiments_store.RemoveAgent: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotSignedUp
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("platform_experiments_store.RemoveAgent: commit: %w", err)
	}
	return nil
}

// The last column is whether the agent's run ended long enough ago for its machine to be
// released: its platform experiment closed, or it was cut.
var agentSignupColumns = `s.platform_experiment_id, s.agent_id, COALESCE(a.name, s.agent_id), s.role, s.quota_tier,
       s.hosted, s.launcher, s.flavor, s.hyperparameters, s.model, s.signed_up_at,
       s.instance_name, s.instance_restarts, s.instance_stopped, s.instance_exited_at, s.instance_exit_log,
       s.instance_seen_at, s.instance_launch,
       s.instance_state, s.instance_progress_at, s.instance_end_reason,
       ` + domain.InstanceReleasedSQL(`LEAST(
           (SELECT ended.closed_at FROM platform_experiments ended WHERE ended.id = s.platform_experiment_id),
           (SELECT cut.cut_at FROM platform_experiment_cuts cut WHERE cut.platform_experiment_id = s.platform_experiment_id AND cut.agent_id = s.agent_id),
           (SELECT planned.proposed_at FROM platform_experiments planned WHERE planned.id = s.platform_experiment_id AND s.flavor = '`+domain.PlannerFlavor+`'))`)

// scanAgentSignup reads agentSignupColumns, and after them whatever else the query selected.
func scanAgentSignup(row pgx.Row, more ...any) (domain.AgentSignup, error) {
	var signup domain.AgentSignup
	var hyperparameters []byte
	var instance domain.AgentInstance
	if err := row.Scan(append([]any{&signup.PlatformExperimentID, &signup.AgentID, &signup.Name, &signup.Role, &signup.QuotaTier,
		&signup.Hosted, &signup.Launcher, &signup.Flavor, &hyperparameters, &signup.Model, &signup.SignedUpAt,
		&instance.Name, &instance.Restarts, &instance.Stopped, &instance.ExitedAt, &instance.ExitLog,
		&instance.SeenAt, &instance.Launch, &instance.Reported, &instance.ProgressAt, &instance.EndReason, &instance.Released}, more...)...); err != nil {
		return domain.AgentSignup{}, err
	}
	if signup.Hosted {
		signup.Instance = &instance
	}
	if err := json.Unmarshal(hyperparameters, &signup.Hyperparameters); err != nil {
		return domain.AgentSignup{}, fmt.Errorf("decode hyperparameters of %s: %w", signup.AgentID, err)
	}
	return signup, nil
}

// ListAgentSignups returns every agent's place in a platform experiment, in signup order.
func (s *PlatformExperimentsStore) ListAgentSignups(ctx context.Context, platformExpID string) ([]domain.AgentSignup, error) {
	rows, err := s.pool.pool.Query(ctx, `
SELECT `+agentSignupColumns+`
FROM experiment_signups s LEFT JOIN agents a ON a.id = s.agent_id
WHERE s.platform_experiment_id = $1
ORDER BY s.signed_up_at, s.agent_id`, platformExpID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.ListAgentSignups: %w", err)
	}
	defer rows.Close()
	signups := []domain.AgentSignup{}
	for rows.Next() {
		signup, err := scanAgentSignup(rows)
		if err != nil {
			return nil, fmt.Errorf("platform_experiments_store.ListAgentSignups: %w", err)
		}
		signups = append(signups, signup)
	}
	return signups, rows.Err()
}

// GetAgentSignup returns one agent's place in a platform experiment, nil when it has none.
func (s *PlatformExperimentsStore) GetAgentSignup(ctx context.Context, platformExpID, agentID string) (*domain.AgentSignup, error) {
	signup, err := scanAgentSignup(s.pool.pool.QueryRow(ctx, `
SELECT `+agentSignupColumns+`
FROM experiment_signups s LEFT JOIN agents a ON a.id = s.agent_id
WHERE s.platform_experiment_id = $1 AND s.agent_id = $2`, platformExpID, agentID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.GetAgentSignup: %w", err)
	}
	return &signup, nil
}

// LauncherAgent is a hosted agent as its launcher's desired state names it. Ending is true once
// its platform experiment closed or it was cut: its machine is no longer wanted and is left to
// finish its last turn.
type LauncherAgent struct {
	domain.AgentSignup
	// AgentImage is the image the agent's platform experiment names for its machines, empty for
	// the platform's own.
	AgentImage string
	Ending     bool
}

// launcherAgentsFrom selects the hosted agents a launcher still has something to do for: those
// of an open or running platform experiment that were not cut, and those whose experiment closed
// or who were cut less than $2 seconds ago. A stopped agent is among them: it is asked for again
// once what it is started with changes. Both moments and the comparison are on the database's
// clock.
//
// A draft's planner is the other way round. It has something to do while the draft is a draft
// and nothing is proposed, which a revision makes true again, and it is let wind down for $2
// seconds after it proposed or the draft closed. Once the draft is accepted it has nothing left
// to do, in an experiment that is then open or running.
const launcherAgentsFrom = `
FROM experiment_signups s
JOIN platform_experiments pe ON pe.id = s.platform_experiment_id
LEFT JOIN agents a ON a.id = s.agent_id
LEFT JOIN platform_experiment_cuts c ON c.platform_experiment_id = s.platform_experiment_id AND c.agent_id = s.agent_id
WHERE s.hosted AND s.launcher = $1
  AND (pe.status = 'closed' AND pe.closed_at > now() - make_interval(secs => $2)
       OR s.flavor <> '` + domain.PlannerFlavor + `' AND pe.status IN ('open', 'running')
       OR s.flavor = '` + domain.PlannerFlavor + `' AND pe.status = 'draft' AND (pe.proposed_at IS NULL OR pe.proposed_at > now() - make_interval(secs => $2)))
  AND (c.agent_id IS NULL OR c.cut_at > now() - make_interval(secs => $2))`

// launcherAgentEnding says that the agent's run is over and its machine is only winding down.
const launcherAgentEnding = `(pe.status = 'closed' OR c.agent_id IS NOT NULL OR (s.flavor = '` + domain.PlannerFlavor + `' AND pe.proposed_at IS NOT NULL))`

// ListLauncherAgents returns the hosted agents the launcher's desired state is decided from.
func (s *PlatformExperimentsStore) ListLauncherAgents(ctx context.Context, launcher string, windDown time.Duration) ([]LauncherAgent, error) {
	rows, err := s.pool.pool.Query(ctx, `
SELECT `+agentSignupColumns+`, pe.agent_image, `+launcherAgentEnding+launcherAgentsFrom+`
ORDER BY s.platform_experiment_id, s.agent_id`, launcher, windDown.Seconds())
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.ListLauncherAgents: %w", err)
	}
	defer rows.Close()
	agents := []LauncherAgent{}
	for rows.Next() {
		var agent LauncherAgent
		var err error
		if agent.AgentSignup, err = scanAgentSignup(rows, &agent.AgentImage, &agent.Ending); err != nil {
			return nil, fmt.Errorf("platform_experiments_store.ListLauncherAgents: %w", err)
		}
		agents = append(agents, agent)
	}
	return agents, rows.Err()
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
