package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// SummaryWrittenAt is when the summary of the platform experiment id was last written, nil while
// it never was.
func (s *PlatformExperimentsStore) SummaryWrittenAt(ctx context.Context, id string) (*time.Time, error) {
	var at *time.Time
	if err := s.pool.pool.QueryRow(ctx, `SELECT summary_at FROM platform_experiments WHERE id = $1`, id).Scan(&at); err != nil {
		return nil, fmt.Errorf("platform_experiments_store.SummaryWrittenAt: %w", err)
	}
	return at, nil
}

// HypothesesInFlight is each hypothesis of the platform experiment id with jobs queued or running
// or a checkout that claims it (domain.CheckoutClaim), the baseline's left out, by hypothesis id,
// with the agents on it.
func (s *PlatformExperimentsStore) HypothesesInFlight(ctx context.Context, id string) ([]domain.HypothesisInFlight, error) {
	rows, err := s.pool.pool.Query(ctx, `
SELECT hypothesis_id, count(*) FILTER (WHERE status <> 'RUNNING'), count(*) FILTER (WHERE status = 'RUNNING'),
       array_agg(DISTINCT agent_id ORDER BY agent_id)
FROM (
	SELECT e.hypothesis_id, e.agent_id, e.status::text AS status FROM experiments e
	WHERE e.platform_experiment_id = $1 AND `+jobOnIt+` AND e.hypothesis_id <> e.platform_experiment_id
	UNION ALL
	SELECT c.hypothesis_id, c.agent_id, NULL FROM hypothesis_checkouts c
	WHERE c.platform_experiment_id = $1 AND `+checkoutClaims+`
) on_it
GROUP BY hypothesis_id ORDER BY hypothesis_id`, id, domain.CheckoutClaim.Seconds())
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.HypothesesInFlight: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.HypothesisInFlight, error) {
		var h domain.HypothesisInFlight
		err := row.Scan(&h.HypothesisID, &h.Queued, &h.Running, &h.AgentIDs)
		return h, err
	})
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.HypothesesInFlight: %w", err)
	}
	return out, nil
}

// IsAgentFinished says the agent finished in the platform experiment.
func (s *PlatformExperimentsStore) IsAgentFinished(ctx context.Context, platformExpID, agentID string) (bool, error) {
	var finished bool
	if err := s.pool.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM experiment_signups
WHERE platform_experiment_id = $1 AND agent_id = $2 AND finished_at IS NOT NULL)`, platformExpID, agentID).Scan(&finished); err != nil {
		return false, fmt.Errorf("platform_experiments_store.IsAgentFinished: %w", err)
	}
	return finished, nil
}

// runEndSQL is why the run of signup s is over, a domain.RunEnd, empty while it goes on.
const runEndSQL = `CASE WHEN (SELECT ended.status = 'closed' FROM platform_experiments ended WHERE ended.id = s.platform_experiment_id) THEN '` + string(domain.RunEndClosed) + `'
            WHEN EXISTS (SELECT 1 FROM platform_experiment_cuts cut WHERE cut.platform_experiment_id = s.platform_experiment_id AND cut.agent_id = s.agent_id) THEN '` + string(domain.RunEndCut) + `'
            WHEN s.finished_at IS NOT NULL THEN '` + string(domain.RunEndFinished) + `'
            WHEN s.role = '` + string(domain.SignupRolePlanner) + `' AND (SELECT planned.planner_ends_at <= now() FROM platform_experiments planned WHERE planned.id = s.platform_experiment_id) THEN '` + string(domain.RunEndPlanned) + `'
            ELSE '' END`

// replacedSQL is whether the platform added a researcher in the place of signup s.
const replacedSQL = `EXISTS (SELECT 1 FROM experiment_signups r WHERE r.platform_experiment_id = s.platform_experiment_id AND r.replaces = s.agent_id)`

// researcherAtWorkSQL is whether signup s is a researcher at work: one that did not finish, was
// not cut, was not replaced and, run by the platform, did not lose its machine with no restart
// left. Each takes one of the places max_agents counts, and only they do: the one count of the
// researchers a platform experiment has, for a signup and for a change of max_agents alike.
const researcherAtWorkSQL = `s.role = '` + string(domain.SignupRoleCompetitor) + `' AND s.finished_at IS NULL AND NOT ` + stoppedSQL + `
  AND NOT EXISTS (SELECT 1 FROM platform_experiment_cuts c WHERE c.platform_experiment_id = s.platform_experiment_id AND c.agent_id = s.agent_id)
  AND NOT ` + replacedSQL

// endedReasonSQL is why signup s no longer holds a place among the researchers, empty while it does.
const endedReasonSQL = `CASE WHEN EXISTS (SELECT 1 FROM platform_experiment_cuts c WHERE c.platform_experiment_id = s.platform_experiment_id AND c.agent_id = s.agent_id) THEN '` + string(domain.RunEndCut) + `'
            WHEN ` + replacedSQL + ` THEN '` + domain.EndedReplaced + `'
            WHEN s.finished_at IS NOT NULL THEN '` + string(domain.RunEndFinished) + `'
            WHEN ` + stoppedSQL + ` THEN '` + domain.EndedStopped + `'
            ELSE '' END`

// The last two columns are whether the agent's run ended long enough ago for its machine to be
// released, and why it ended: its platform experiment closed, it was cut, it finished, or it is
// a planner whose answer is stored.
var agentSignupColumns = `s.platform_experiment_id, s.agent_id, COALESCE(a.name, s.agent_id), s.role, s.quota_tier,
       s.hosted, s.agent_type, s.hyperparameters, COALESCE(s.model, ''), s.signed_up_at,
       ` + instanceColumns + `,
       s.finished_at, s.finished_reason, s.added_by_kind, s.added_by_id, s.replaces,
       COALESCE((SELECT r.agent_id FROM experiment_signups r WHERE r.platform_experiment_id = s.platform_experiment_id AND r.replaces = s.agent_id), ''),
       ` + domain.InstanceReleasedSQL(`LEAST(
           s.finished_at,
           (SELECT ended.closed_at FROM platform_experiments ended WHERE ended.id = s.platform_experiment_id),
           (SELECT cut.cut_at FROM platform_experiment_cuts cut WHERE cut.platform_experiment_id = s.platform_experiment_id AND cut.agent_id = s.agent_id),
           (SELECT planned.planner_ends_at FROM platform_experiments planned WHERE planned.id = s.platform_experiment_id AND s.role = '`+string(domain.SignupRolePlanner)+`'))`) + `,
       ` + runEndSQL + `,
       COALESCE((SELECT cut.reason FROM platform_experiment_cuts cut WHERE cut.platform_experiment_id = s.platform_experiment_id AND cut.agent_id = s.agent_id), ''),
       (` + researcherAtWorkSQL + `), ` + endedReasonSQL

// scanAgentSignup reads agentSignupColumns, and after them whatever else the query selected.
func scanAgentSignup(row pgx.Row, more ...any) (domain.AgentSignup, error) {
	var signup domain.AgentSignup
	var hyperparameters []byte
	var instance domain.AgentInstance
	addedBy := domain.Actor{}
	var activity string
	var activityAt *time.Time
	if err := row.Scan(append([]any{&signup.PlatformExperimentID, &signup.AgentID, &signup.Name, &signup.Role, &signup.QuotaTier,
		&signup.Hosted, &signup.AgentType, &hyperparameters, &signup.Model, &signup.SignedUpAt,
		&instance.Name, &instance.Restarts, &instance.Renewals, &instance.Stopped, &instance.ExitedAt, &instance.ExitLog,
		&instance.SeenAt, &instance.AskedAt, &instance.Launch, &instance.Reported, &instance.ProgressAt, &instance.EndReason, &activity, &activityAt,
		&signup.FinishedAt, &signup.FinishedReason, &addedBy.Kind, &addedBy.ID, &signup.Replaces, &signup.ReplacedBy, &instance.Released, &signup.Ended, &signup.CutReason, &signup.AtWork, &signup.EndedReason}, more...)...); err != nil {
		return domain.AgentSignup{}, err
	}
	signup.Actor = &addedBy
	if signup.Hosted {
		signup.Instance = &instance
		signup.Activity = domain.ActivityOf(activity, activityAt)
	}
	if hyperparameters == nil {
		return signup, nil
	}
	if err := json.Unmarshal(hyperparameters, &signup.Hyperparameters); err != nil {
		return domain.AgentSignup{}, fmt.Errorf("decode hyperparameters of %s: %w", signup.AgentID, err)
	}
	return signup, nil
}

// ListAgentSignups returns every agent's place in a platform experiment, in signup order, each
// with what of its own is in flight.
func (s *PlatformExperimentsStore) ListAgentSignups(ctx context.Context, platformExpID string) ([]domain.AgentSignup, error) {
	rows, err := s.pool.pool.Query(ctx, `
SELECT `+agentSignupColumns+`, f.jobs, f.running,
       CASE WHEN pe.status = 'running' AND pe.started_at IS NOT NULL AND f.jobs = 0 THEN `+idleSinceSQL+` END
FROM experiment_signups s LEFT JOIN agents a ON a.id = s.agent_id`+signupInstanceJoin+`
JOIN platform_experiments pe ON pe.id = s.platform_experiment_id
CROSS JOIN LATERAL (SELECT count(*) AS jobs, count(*) FILTER (WHERE e.status = 'RUNNING') AS running
                    FROM experiments e
                    WHERE e.platform_experiment_id = s.platform_experiment_id AND e.agent_id = s.agent_id
                      AND e.status IN ('QUEUED', 'SUBMITTED', 'RUNNING')) f
WHERE s.platform_experiment_id = $1
ORDER BY s.signed_up_at, s.agent_id`, platformExpID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.ListAgentSignups: %w", err)
	}
	defer rows.Close()
	signups := []domain.AgentSignup{}
	for rows.Next() {
		var inFlight domain.InFlight
		signup, err := scanAgentSignup(rows, &inFlight.Jobs, &inFlight.Running, &inFlight.IdleSince)
		if err != nil {
			return nil, fmt.Errorf("platform_experiments_store.ListAgentSignups: %w", err)
		}
		inFlight.Queued = inFlight.Jobs - inFlight.Running
		signup.InFlight = &inFlight
		signups = append(signups, signup)
	}
	return signups, rows.Err()
}

// GetAgentSignup returns one agent's place in a platform experiment, nil when it has none.
func (s *PlatformExperimentsStore) GetAgentSignup(ctx context.Context, platformExpID, agentID string) (*domain.AgentSignup, error) {
	signup, err := scanAgentSignup(s.pool.pool.QueryRow(ctx, `
SELECT `+agentSignupColumns+`
FROM experiment_signups s LEFT JOIN agents a ON a.id = s.agent_id`+signupInstanceJoin+`
WHERE s.platform_experiment_id = $1 AND s.agent_id = $2`, platformExpID, agentID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.GetAgentSignup: %w", err)
	}
	return &signup, nil
}

// LauncherAgent is a hosted agent as its launcher's desired state names it. Once its run Ended
// its machine is no longer wanted and is left to finish its last turn.
type LauncherAgent struct {
	domain.AgentSignup
	// AgentImage is the image the agent's platform experiment names for its machines, empty for
	// the platform's own.
	AgentImage string
}

// launcherAgentsFrom selects the hosted agents the launcher still has something to do for: those
// of a running platform experiment that were not cut and did not finish, those that measure the
// baseline of an open one, since no researcher's machine is rented before there is a baseline to
// start from, and those
// whose experiment closed, who were cut or who finished less than $1 seconds ago. A stopped agent is among them: it is asked for again
// once what it is started with changes. Both moments and the comparison are on the database's
// clock.
//
// None of them has anything to do while the platform still creates the repository they clone
// (awaitsRepository).
//
// A draft's baseline agent, which its planner's proposal signed up, has something to do while the
// platform is to accept that proposal by itself (accepts_at): its machine starts then, so that it
// is up once the draft is accepted. A draft that waits for a person keeps no machine of it.
//
// A draft's planner is the other way round. It has something to do while the draft is a draft
// and nothing is proposed, which a revision makes true again. After its answer it is kept until
// the draft's planner_ends_at, for a revision asked for soon, and then let wind down for $1
// seconds, as it is after the draft closed. Once the draft is accepted it has nothing left to
// do, in an experiment that is then open or running.
//
// A researcher the platform replaced has nothing left to do either: one whose machine stopped
// would otherwise be started again once what it is started with changes, beside its replacement.
const launcherAgentsFrom = `
FROM experiment_signups s
JOIN platform_experiments pe ON pe.id = s.platform_experiment_id
LEFT JOIN agents a ON a.id = s.agent_id` + signupInstanceJoin + `
LEFT JOIN platform_experiment_cuts c ON c.platform_experiment_id = s.platform_experiment_id AND c.agent_id = s.agent_id
WHERE s.hosted AND NOT (` + awaitsRepository + `)
  AND (pe.status = 'closed' AND pe.closed_at > now() - make_interval(secs => $1)
       OR s.role <> '` + string(domain.SignupRolePlanner) + `' AND (pe.status = 'running' OR pe.status = 'open' AND s.role = '` + string(domain.SignupRoleBaseline) + `')
       OR s.role = '` + string(domain.SignupRoleBaseline) + `' AND pe.status = 'draft' AND pe.accepts_at IS NOT NULL
       OR s.role = '` + string(domain.SignupRolePlanner) + `' AND pe.status = 'draft' AND (pe.planner_ends_at IS NULL OR pe.planner_ends_at > now() - make_interval(secs => $1)))
  AND (c.agent_id IS NULL OR c.cut_at > now() - make_interval(secs => $1))
  AND (s.finished_at IS NULL OR s.finished_at > now() - make_interval(secs => $1))
  AND NOT ` + replacedSQL

// ListLauncherAgents returns the hosted agents the launcher's desired state is decided from.
func (s *PlatformExperimentsStore) ListLauncherAgents(ctx context.Context, windDown time.Duration) ([]LauncherAgent, error) {
	rows, err := s.pool.pool.Query(ctx, `
SELECT `+agentSignupColumns+`, pe.agent_image`+launcherAgentsFrom+`
ORDER BY s.platform_experiment_id, s.agent_id`, windDown.Seconds())
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.ListLauncherAgents: %w", err)
	}
	defer rows.Close()
	agents := []LauncherAgent{}
	for rows.Next() {
		var agent LauncherAgent
		var err error
		if agent.AgentSignup, err = scanAgentSignup(rows, &agent.AgentImage); err != nil {
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

// ListParticipants returns the agents taking part in a platform experiment: every signup but
// the planner of a draft that was accepted, whose work ended there.
func (s *PlatformExperimentsStore) ListParticipants(ctx context.Context, platformExpID string) ([]string, error) {
	rows, err := s.pool.pool.Query(ctx, `
SELECT s.agent_id FROM experiment_signups s JOIN platform_experiments pe ON pe.id = s.platform_experiment_id
WHERE s.platform_experiment_id = $1 AND (pe.status = 'draft' OR s.role <> '`+string(domain.SignupRolePlanner)+`')
ORDER BY s.signed_up_at ASC`, platformExpID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.ListParticipants: %w", err)
	}
	agents, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.ListParticipants: %w", err)
	}
	return agents, nil
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
		return domain.DataKey{}, domain.ErrNotSignedUp
	}
	if err != nil {
		return domain.DataKey{}, fmt.Errorf("platform_experiments_store.AgentDataKey: %w", err)
	}
	if status == string(domain.PlatformExpClosed) {
		return domain.DataKey{}, domain.ErrPlatformExperimentClosed
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
