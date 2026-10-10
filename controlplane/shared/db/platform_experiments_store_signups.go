package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// ---- experiment_signups ----

// ErrMaxAgentsReached is a researcher's signup refused because max_agents researchers joined.
var ErrMaxAgentsReached = errors.New("max_agents reached")

// ErrMaxHostedReached is returned when a platform experiment would have the platform run more
// machines than its max_hosted, or leave none of them to measure its baseline.
var ErrMaxHostedReached = errors.New("max_hosted reached")

// ErrAgentNameTaken is an agent refused a place because another agent of the platform experiment
// carries its name.
var ErrAgentNameTaken = errors.New("another agent of the platform experiment has this name")

// ErrSignupClosed is a signup or a removal refused because the platform experiment is no longer
// open, or does not exist. Once it runs only a researcher joins it (JoinRunning).
var ErrSignupClosed = errors.New("platform experiment is not open")

// ErrNoShareLeft is a researcher refused a place in a running platform experiment because its
// even part of what the researchers left unspent would not hold one trial (LateJoin.MinShare).
var ErrNoShareLeft = errors.New("no unspent share left to give a researcher")

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
	inserted, err := s.enrol(ctx, platformExpID, nil, []domain.AgentSignup{signup}, nil)
	if errors.Is(err, ErrSignupClosed) {
		return false, nil
	}
	return inserted == 1, err
}

// SpawnAgent creates an agent in the platform experiment's org and signs it up, both or neither.
// An agent without a name is given one (domain.AgentName), which agent then carries.
// ErrAgentNameTaken when its name is another agent's in the experiment. ErrSignupClosed when the experiment is not open, ErrMaxAgentsReached when it is a competitor
// with no place left, ErrMaxHostedReached when it is one machine too many.
func (s *PlatformExperimentsStore) SpawnAgent(ctx context.Context, platformExpID string, agent *domain.Agent, signup domain.AgentSignup) error {
	_, err := s.enrol(ctx, platformExpID, []*domain.Agent{agent}, []domain.AgentSignup{signup}, nil)
	return err
}

// LateJoin is what a researcher that joins a running platform experiment is given its quota
// from (domain.LateShare).
type LateJoin struct {
	// Used is what each researcher of the platform experiment has used of its quota, the jobs
	// that have not ended at their estimate.
	Used map[string]float64
	// BurstFraction sizes the newcomer's burst hours from its share, as at the start.
	BurstFraction float64
	// MinShare is the least share worth giving: what one trial of the baseline holds.
	MinShare float64
}

// JoinRunning gives a researcher a place in a running platform experiment, and its quota: a fair
// part of what the researchers left unspent, taken first from the ones that ended (domain.LateShare).
// agent is created first when not nil. The errors are SpawnAgent's, ErrSignupClosed for any
// other role or a platform experiment that is not running, and ErrNoShareLeft.
func (s *PlatformExperimentsStore) JoinRunning(ctx context.Context, platformExpID string, agent *domain.Agent, signup domain.AgentSignup, late LateJoin) error {
	var create []*domain.Agent
	if agent != nil {
		create = []*domain.Agent{agent}
	}
	inserted, err := s.enrol(ctx, platformExpID, create, []domain.AgentSignup{signup}, &late)
	if err == nil && inserted == 0 {
		return ErrSignupClosed
	}
	return err
}

// enrol is the one transaction a signup row is written in. create is inserted first, into the org
// the platform experiment belongs to.
func (s *PlatformExperimentsStore) enrol(ctx context.Context, platformExpID string, create []*domain.Agent, signups []domain.AgentSignup, late *LateJoin) (int, error) {
	tx, err := s.pool.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("platform_experiments_store.Signup: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	inserted, err := enrolTx(ctx, tx, platformExpID, create, signups, late)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("platform_experiments_store.Signup: commit: %w", err)
	}
	return inserted, nil
}

// allocateBaselineQuota writes what an agent of role baseline holds while its platform experiment
// is open: half the budget (domain.BaselineShare), in the column its tier names
// (domain.ApplyQuotaTier). It is written when the agent joins and again whenever the budget of
// the open experiment changes.
func allocateBaselineQuota(ctx context.Context, tx pgx.Tx, platformExpID, agentID string, tier domain.QuotaTier, budget float64, defaultTier domain.QuotaTier) error {
	guaranteed, burst := domain.ApplyQuotaTier(domain.ResolveQuotaTier(tier, defaultTier), domain.BaselineShare(budget), 0)
	id, err := domain.NewID(domain.AgentQuotaIDPrefix)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, upsertAgentQuotaSQL, agentQuotaUpsertArgs(&domain.AgentQuota{
		ID: id, AgentID: agentID, PlatformExperimentID: platformExpID,
		GuaranteedAcceleratorHours: guaranteed, BurstAcceleratorHours: burst, CreatedAt: time.Now().UTC(),
	})...); err != nil {
		return fmt.Errorf("allocate the quota of baseline agent %s: %w", agentID, err)
	}
	return nil
}

// enrolTx is enrol inside the caller's transaction. With late it adds researchers to a running
// platform experiment, and to nothing else.
//
// An agent of role baseline is given its quota here, with its place: its job runs while the
// platform experiment is open, before the start divides the budget (allocateBaselineQuota). So
// is a researcher that joins a running one (allocateLateQuota).
func enrolTx(ctx context.Context, tx pgx.Tx, platformExpID string, create []*domain.Agent, signups []domain.AgentSignup, late *LateJoin) (int, error) {
	var status, orgID string
	var maxAgents, maxHosted int
	var coordinatorHosted bool
	var budget float64
	var defaultTier domain.QuotaTier
	if err := tx.QueryRow(ctx, `
SELECT pe.status, pe.max_agents, pe.max_hosted, pe.coordinator_hosted, p.org_id, pe.budget_accelerator_hours, pe.default_quota_tier
FROM platform_experiments pe JOIN projects p ON p.id = pe.project_id
WHERE pe.id=$1 FOR UPDATE OF pe`, platformExpID).Scan(&status, &maxAgents, &maxHosted, &coordinatorHosted, &orgID, &budget, &defaultTier); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, ErrSignupClosed
		}
		return 0, fmt.Errorf("platform_experiments_store.Signup: lock experiment: %w", err)
	}
	running := late != nil
	if running && (status != string(domain.PlatformExpRunning) || slices.ContainsFunc(signups, func(signup domain.AgentSignup) bool { return signup.Role != domain.SignupRoleCompetitor })) {
		return 0, ErrSignupClosed
	}
	if !running && status != string(domain.PlatformExpOpen) {
		return 0, ErrSignupClosed
	}
	// Names are unique in a platform experiment, among the agents in it and those that left a
	// job in it. The experiment row is locked, so two agents added at once cannot share one.
	names, err := agentNamesTx(ctx, tx, platformExpID)
	if err != nil {
		return 0, fmt.Errorf("platform_experiments_store.Signup: %w", err)
	}
	claim := func(agentID, name string) error {
		if holder, taken := names[name]; taken && holder != agentID {
			return ErrAgentNameTaken
		}
		names[name] = agentID
		return nil
	}
	roles := make(map[string]domain.SignupRole, len(signups))
	for _, signup := range signups {
		roles[signup.AgentID] = signup.Role
	}
	for _, agent := range create {
		agent.OrgID = orgID
		if agent.Name == "" {
			agent.Name = domain.AgentName(roles[agent.ID], func(name string) bool { _, taken := names[name]; return taken })
		}
		if err := claim(agent.ID, agent.Name); err != nil {
			return 0, err
		}
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
	var joined []domain.AgentSignup
	for _, signup := range signups {
		if !created[signup.AgentID] {
			var agentOrg, agentName string
			err := tx.QueryRow(ctx, `SELECT org_id, name FROM agents WHERE id=$1`, signup.AgentID).Scan(&agentOrg, &agentName)
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && agentOrg != orgID) {
				return 0, ErrAgentOutsideOrg
			}
			if err != nil {
				return 0, fmt.Errorf("platform_experiments_store.Signup: read agent %s: %w", signup.AgentID, err)
			}
			if err := claim(signup.AgentID, agentName); err != nil {
				return 0, err
			}
		}
		hyperparameters, err := json.Marshal(nonNilObject(signup.Hyperparameters))
		if err != nil {
			return 0, fmt.Errorf("platform_experiments_store.Signup: encode hyperparameters: %w", err)
		}
		var addedBy domain.Actor
		if signup.Actor != nil {
			addedBy = *signup.Actor
		}
		tag, err := tx.Exec(ctx, `
INSERT INTO experiment_signups (platform_experiment_id, agent_id, signed_up_at, role, quota_tier, hosted, launcher, flavor, hyperparameters, model, added_by_kind, added_by_id)
VALUES ($1, $2, NOW(), $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT (platform_experiment_id, agent_id) DO NOTHING`, platformExpID, signup.AgentID, string(signup.Role), string(signup.QuotaTier),
			signup.Hosted, signup.Launcher, signup.Flavor, hyperparameters, signup.Model, string(addedBy.Kind), addedBy.ID)
		if err != nil {
			return 0, fmt.Errorf("platform_experiments_store.Signup: %w", err)
		}
		if tag.RowsAffected() == 0 {
			continue
		}
		inserted++
		competitors = competitors || signup.Role == domain.SignupRoleCompetitor
		hosted = hosted || signup.Hosted
		if running {
			joined = append(joined, signup)
			continue
		}
		if signup.Role != domain.SignupRoleBaseline {
			continue
		}
		if err := allocateBaselineQuota(ctx, tx, platformExpID, signup.AgentID, signup.QuotaTier, budget, defaultTier); err != nil {
			return 0, fmt.Errorf("platform_experiments_store.Signup: %w", err)
		}
	}
	// max_hosted bounds the machines the platform rents for the experiment at once. An agent that
	// finished holds none, nor does one whose machine stopped with no restart left, and a planner
	// is no agent of an open experiment. One machine is kept for measuring the baseline, which
	// nothing starts without: the coordinator and the agents that do not measure it take the
	// others. A baseline agent whose machine stopped leaves that one to the next.
	if hosted || (len(signups) == 0 && coordinatorHosted) {
		var measuring, others int
		if err := tx.QueryRow(ctx, `
SELECT COUNT(*) FILTER (WHERE role = $2), COUNT(*) FILTER (WHERE role <> $2)
FROM experiment_signups
WHERE platform_experiment_id = $1 AND hosted AND finished_at IS NULL AND NOT instance_stopped AND role <> $3`,
			platformExpID, string(domain.SignupRoleBaseline), string(domain.SignupRolePlanner)).Scan(&measuring, &others); err != nil {
			return 0, fmt.Errorf("platform_experiments_store.Signup: count hosted: %w", err)
		}
		if coordinatorHosted {
			others++
		}
		if others > maxHosted-1 || measuring+others > maxHosted {
			return 0, ErrMaxHostedReached
		}
	}
	if inserted == 0 {
		return 0, nil
	}
	// max_agents bounds the researchers at work at once: one that finished, was cut or whose
	// machine stopped with no restart left leaves its place to another, and nobody of another
	// role takes one.
	if competitors {
		var count int
		if err := tx.QueryRow(ctx, `
SELECT COUNT(*) FROM experiment_signups s
WHERE s.platform_experiment_id = $1 AND s.role = $2 AND s.finished_at IS NULL AND NOT (s.hosted AND s.instance_stopped)
  AND NOT EXISTS (SELECT 1 FROM platform_experiment_cuts c WHERE c.platform_experiment_id = s.platform_experiment_id AND c.agent_id = s.agent_id)`,
			platformExpID, string(domain.SignupRoleCompetitor)).Scan(&count); err != nil {
			return 0, fmt.Errorf("platform_experiments_store.Signup: count competitors: %w", err)
		}
		if count > maxAgents {
			return 0, ErrMaxAgentsReached
		}
	}
	for _, signup := range joined {
		if err := allocateLateQuota(ctx, tx, platformExpID, signup, *late, defaultTier); err != nil {
			return 0, err
		}
	}
	return inserted, nil
}

// allocateLateQuota gives signup, a researcher joining the running platform experiment, its share
// of what the researchers left unspent (domain.LateShare), and takes that share from the quotas of
// the others, never below it nor below what they used. The experiment row is locked, so two researchers joining at once do not
// take the same hours.
func allocateLateQuota(ctx context.Context, tx pgx.Tx, platformExpID string, signup domain.AgentSignup, late LateJoin, defaultTier domain.QuotaTier) error {
	rows, err := tx.Query(ctx, `
SELECT q.agent_id, q.guaranteed_accelerator_hours, q.burst_accelerator_hours,
       s.finished_at IS NOT NULL OR (s.hosted AND s.instance_stopped)
FROM agent_quotas q
JOIN experiment_signups s ON s.platform_experiment_id = q.platform_experiment_id AND s.agent_id = q.agent_id
WHERE q.platform_experiment_id = $1 AND s.role = $2 AND s.agent_id <> $3
  AND NOT EXISTS (SELECT 1 FROM platform_experiment_cuts c WHERE c.platform_experiment_id = q.platform_experiment_id AND c.agent_id = q.agent_id)
ORDER BY s.signed_up_at, s.agent_id`, platformExpID, string(domain.SignupRoleCompetitor), signup.AgentID)
	if err != nil {
		return fmt.Errorf("platform_experiments_store.JoinRunning: read quotas: %w", err)
	}
	var quotas []domain.ResearcherQuota
	for rows.Next() {
		var q domain.ResearcherQuota
		if err := rows.Scan(&q.AgentID, &q.Guaranteed, &q.Burst, &q.Ended); err != nil {
			rows.Close()
			return fmt.Errorf("platform_experiments_store.JoinRunning: read quotas: %w", err)
		}
		q.Used = late.Used[q.AgentID]
		quotas = append(quotas, q)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("platform_experiments_store.JoinRunning: read quotas: %w", err)
	}
	share, taken := domain.LateShare(quotas)
	if share <= 0 || share < late.MinShare {
		return ErrNoShareLeft
	}
	for agentID, take := range taken {
		if _, err := tx.Exec(ctx, `
UPDATE agent_quotas SET
    guaranteed_accelerator_hours = CASE WHEN guaranteed_accelerator_hours > 0 THEN GREATEST(0, guaranteed_accelerator_hours - $3) ELSE 0 END,
    burst_accelerator_hours = CASE WHEN guaranteed_accelerator_hours > 0 THEN burst_accelerator_hours ELSE GREATEST(0, burst_accelerator_hours - $3) END
WHERE platform_experiment_id = $1 AND agent_id = $2`, platformExpID, agentID, take); err != nil {
			return fmt.Errorf("platform_experiments_store.JoinRunning: take from %s: %w", agentID, err)
		}
	}
	guaranteed, burst := domain.ApplyQuotaTier(domain.ResolveQuotaTier(signup.QuotaTier, defaultTier), share, domain.RoundHours(share*late.BurstFraction))
	id, err := domain.NewID(domain.AgentQuotaIDPrefix)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, upsertAgentQuotaSQL, agentQuotaUpsertArgs(&domain.AgentQuota{
		ID: id, AgentID: signup.AgentID, PlatformExperimentID: platformExpID,
		GuaranteedAcceleratorHours: guaranteed, BurstAcceleratorHours: burst, CreatedAt: time.Now().UTC(),
	})...); err != nil {
		return fmt.Errorf("platform_experiments_store.JoinRunning: allocate the quota of %s: %w", signup.AgentID, err)
	}
	return nil
}

// agentNamesTx returns the names carried in a platform experiment, each with its agent: of the
// agents signed up to it and of those that have a job in it.
func agentNamesTx(ctx context.Context, tx pgx.Tx, platformExpID string) (map[string]string, error) {
	rows, err := tx.Query(ctx, `
SELECT a.name, a.id FROM agents a
WHERE a.id IN (SELECT agent_id FROM experiment_signups WHERE platform_experiment_id = $1)
   OR a.id IN (SELECT agent_id FROM experiments WHERE platform_experiment_id = $1)`, platformExpID)
	if err != nil {
		return nil, fmt.Errorf("list agent names: %w", err)
	}
	defer rows.Close()
	names := map[string]string{}
	for rows.Next() {
		var name, id string
		if err := rows.Scan(&name, &id); err != nil {
			return nil, fmt.Errorf("list agent names: %w", err)
		}
		names[name] = id
	}
	return names, rows.Err()
}

func nonNilObject(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// RemoveAgent takes an agent out of a platform experiment that has not started. ErrSignupClosed
// once it has, ErrNotSignedUp when the agent is not in it, ErrWorkUnfinished while a job of it is
// queued or running, which only an agent of role baseline has then. Only its place goes, and the quota a
// baseline agent held with it: the agent itself
// stays in its org however it came to be, as every agent that ever joined does, and can be added
// again by its id. The experiment row is locked as in
// enrol, so a removal and a start cannot both believe they came first. The signup holds the only
// record of the agent's object-store key, so deleteDataKey takes that key out of the store before
// the row goes, and a key that could not be deleted keeps the signup.
func (s *PlatformExperimentsStore) RemoveAgent(ctx context.Context, platformExpID, agentID string, deleteDataKey func(ctx context.Context, accessKeyID string) error) error {
	tx, err := s.pool.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("platform_experiments_store.RemoveAgent: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// A submission of this agent in flight commits or rolls back first, as in FinishAgent, so the
	// jobs read below include it, and one that starts later finds no quota to be admitted on.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, admissionLockKey(agentID, platformExpID)); err != nil {
		return fmt.Errorf("platform_experiments_store.RemoveAgent: lock: %w", err)
	}
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
	var accessKeyID string
	var inFlight bool
	err = tx.QueryRow(ctx, `
SELECT s.data_access_key_id,
       EXISTS (SELECT 1 FROM experiments
               WHERE platform_experiment_id = s.platform_experiment_id AND agent_id = s.agent_id AND status IN ('QUEUED', 'SUBMITTED', 'RUNNING'))
FROM experiment_signups s
WHERE s.platform_experiment_id=$1 AND s.agent_id=$2 FOR UPDATE OF s`, platformExpID, agentID).Scan(&accessKeyID, &inFlight)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotSignedUp
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
	if _, err := tx.Exec(ctx, `DELETE FROM experiment_signups WHERE platform_experiment_id=$1 AND agent_id=$2`, platformExpID, agentID); err != nil {
		return fmt.Errorf("platform_experiments_store.RemoveAgent: %w", err)
	}
	// Only an agent of role baseline holds one before the start.
	if _, err := tx.Exec(ctx, `DELETE FROM agent_quotas WHERE platform_experiment_id=$1 AND agent_id=$2`, platformExpID, agentID); err != nil {
		return fmt.Errorf("platform_experiments_store.RemoveAgent: release quota: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("platform_experiments_store.RemoveAgent: commit: %w", err)
	}
	return nil
}

// ErrWorkUnfinished refuses finishing or removing an agent that still has a job queued or
// running.
var ErrWorkUnfinished = errors.New("the agent has a job in flight")

// FinishAgent records that an agent of a running platform experiment has nothing left to do, or
// that one that measures the baseline is done in an open one, where its work is. It answers
// ErrSignupClosed anywhere else, ErrNotSignedUp, and ErrWorkUnfinished. An agent that already
// finished keeps the moment and the reason it has.
func (s *PlatformExperimentsStore) FinishAgent(ctx context.Context, platformExpID, agentID, reason string) error {
	tx, err := s.pool.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("platform_experiments_store.FinishAgent: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

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
		return ErrNotSignedUp
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
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("platform_experiments_store.FinishAgent: commit: %w", err)
	}
	return nil
}

// idleSinceSQL is when the stretch of signup s of platform experiment pe with nothing of its own
// on the platform began: the start of the run, its joining, or the last moment a job of its was
// created or seen running, whichever is latest. It says nothing of whether a job is in flight now.
const idleSinceSQL = `GREATEST(pe.started_at, s.signed_up_at,
                    (SELECT max(GREATEST(e.created_at, e.run_reported_at)) FROM experiments e
                     WHERE e.platform_experiment_id = s.platform_experiment_id AND e.agent_id = s.agent_id))`

// IdleResearcher is a researcher of a running platform experiment with nothing of its own on the
// platform for at least the notice, and due to be told so (agent.idle).
type IdleResearcher struct {
	PlatformExperimentID string
	AgentID              string
	// Since is when the stretch began (idleSinceSQL).
	Since time.Time
	// Repeat is 0 for the first notice of the stretch, and counts the notices of it told before.
	Repeat int
	// NoticedAt is when it was last told, nil for never. The notice stands only while it still is.
	NoticedAt *time.Time
}

// IdleResearchers is every researcher of a running platform experiment that had no job of its
// own created or running for after, since the run started or it joined, has none queued or
// running, and was not told so in the last after of the stretch: due a notice. A researcher that
// finished or was cut is never due one.
func (s *PlatformExperimentsStore) IdleResearchers(ctx context.Context, after time.Duration) ([]IdleResearcher, error) {
	rows, err := s.pool.pool.Query(ctx, `
WITH idle AS (
    SELECT s.platform_experiment_id, s.agent_id, s.idle_noticed_at, s.idle_repeat, `+idleSinceSQL+` AS since
    FROM experiment_signups s JOIN platform_experiments pe ON pe.id = s.platform_experiment_id
    WHERE pe.status = 'running' AND pe.started_at IS NOT NULL AND s.role = $2 AND s.finished_at IS NULL
      AND NOT EXISTS (SELECT 1 FROM platform_experiment_cuts c
                      WHERE c.platform_experiment_id = s.platform_experiment_id AND c.agent_id = s.agent_id)
      AND NOT EXISTS (SELECT 1 FROM experiments e
                      WHERE e.platform_experiment_id = s.platform_experiment_id AND e.agent_id = s.agent_id
                        AND e.status IN ('QUEUED', 'SUBMITTED', 'RUNNING'))
)
SELECT platform_experiment_id, agent_id, since,
       CASE WHEN idle_noticed_at IS NULL OR idle_noticed_at < since THEN 0 ELSE idle_repeat + 1 END,
       idle_noticed_at
FROM idle
WHERE since <= now() - make_interval(secs => $1)
  AND (idle_noticed_at IS NULL OR idle_noticed_at < GREATEST(since, now() - make_interval(secs => $1)))
ORDER BY platform_experiment_id, agent_id`, after.Seconds(), string(domain.SignupRoleCompetitor))
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.IdleResearchers: %w", err)
	}
	idle, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (IdleResearcher, error) {
		var r IdleResearcher
		err := row.Scan(&r.PlatformExperimentID, &r.AgentID, &r.Since, &r.Repeat, &r.NoticedAt)
		return r, err
	})
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.IdleResearchers: %w", err)
	}
	return idle, nil
}

// NoticeIdle tells r that it is idle, with detail: agent.idle, new for its first notice of the
// stretch and continues after. A notice told since r was read stands, and this one writes nothing.
func (s *PlatformExperimentsStore) NoticeIdle(ctx context.Context, r IdleResearcher, detail string) error {
	if _, err := s.pool.pool.Exec(ctx, `
UPDATE experiment_signups
SET idle_noticed_at = now(), idle_xid = pg_current_xact_id()::text::bigint, idle_repeat = $3, idle_detail = $4
WHERE platform_experiment_id = $1 AND agent_id = $2 AND idle_noticed_at IS NOT DISTINCT FROM $5`,
		r.PlatformExperimentID, r.AgentID, r.Repeat, detail, r.NoticedAt); err != nil {
		return fmt.Errorf("platform_experiments_store.NoticeIdle: %w", err)
	}
	return nil
}

// SetCloseAgents keeps lines as how each agent's run of the platform experiment id stands at its
// close, for its record of the close. It is taken just before the close is written, and one that
// is closed keeps what it has.
func (s *PlatformExperimentsStore) SetCloseAgents(ctx context.Context, id, lines string) error {
	if _, err := s.pool.pool.Exec(ctx, `UPDATE platform_experiments SET close_agents = $2 WHERE id = $1 AND status <> 'closed'`, id, lines); err != nil {
		return fmt.Errorf("platform_experiments_store.SetCloseAgents: %w", err)
	}
	return nil
}

// CloseAgents is what SetCloseAgents kept of the platform experiment id, empty when nothing was.
func (s *PlatformExperimentsStore) CloseAgents(ctx context.Context, id string) (string, error) {
	var lines string
	if err := s.pool.pool.QueryRow(ctx, `SELECT close_agents FROM platform_experiments WHERE id = $1`, id).Scan(&lines); err != nil {
		return "", fmt.Errorf("platform_experiments_store.CloseAgents: %w", err)
	}
	return lines, nil
}

// SummaryWrittenAt is when the summary of the platform experiment id was last written, nil while
// it never was.
func (s *PlatformExperimentsStore) SummaryWrittenAt(ctx context.Context, id string) (*time.Time, error) {
	var at *time.Time
	if err := s.pool.pool.QueryRow(ctx, `SELECT summary_at FROM platform_experiments WHERE id = $1`, id).Scan(&at); err != nil {
		return nil, fmt.Errorf("platform_experiments_store.SummaryWrittenAt: %w", err)
	}
	return at, nil
}

// HypothesesInFlight is each hypothesis of the platform experiment id with jobs queued or running,
// the baseline's left out, by hypothesis id, with the agents whose jobs they are.
func (s *PlatformExperimentsStore) HypothesesInFlight(ctx context.Context, id string) ([]domain.HypothesisInFlight, error) {
	rows, err := s.pool.pool.Query(ctx, `
SELECT hypothesis_id, count(*) FILTER (WHERE status <> 'RUNNING'), count(*) FILTER (WHERE status = 'RUNNING'),
       array_agg(DISTINCT agent_id ORDER BY agent_id)
FROM experiments
WHERE platform_experiment_id = $1 AND status IN ('QUEUED', 'SUBMITTED', 'RUNNING') AND hypothesis_id <> platform_experiment_id
GROUP BY hypothesis_id ORDER BY hypothesis_id`, id)
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

// AgentFinishedAt returns when the agent finished in the platform experiment, nil while it has
// not, and for an agent that is not in it.
func (s *PlatformExperimentsStore) AgentFinishedAt(ctx context.Context, platformExpID, agentID string) (*time.Time, error) {
	var finishedAt *time.Time
	err := s.pool.pool.QueryRow(ctx, `SELECT finished_at FROM experiment_signups WHERE platform_experiment_id = $1 AND agent_id = $2`, platformExpID, agentID).Scan(&finishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.AgentFinishedAt: %w", err)
	}
	return finishedAt, nil
}

// runEndSQL is why the run of signup s is over, a domain.RunEnd, empty while it goes on.
const runEndSQL = `CASE WHEN (SELECT ended.status = 'closed' FROM platform_experiments ended WHERE ended.id = s.platform_experiment_id) THEN '` + string(domain.RunEndClosed) + `'
            WHEN EXISTS (SELECT 1 FROM platform_experiment_cuts cut WHERE cut.platform_experiment_id = s.platform_experiment_id AND cut.agent_id = s.agent_id) THEN '` + string(domain.RunEndCut) + `'
            WHEN s.finished_at IS NOT NULL THEN '` + string(domain.RunEndFinished) + `'
            WHEN s.role = '` + string(domain.SignupRolePlanner) + `' AND (SELECT planned.planner_ends_at <= now() FROM platform_experiments planned WHERE planned.id = s.platform_experiment_id) THEN '` + string(domain.RunEndPlanned) + `'
            ELSE '' END`

// The last two columns are whether the agent's run ended long enough ago for its machine to be
// released, and why it ended: its platform experiment closed, it was cut, it finished, or it is
// a planner whose answer is stored.
var agentSignupColumns = `s.platform_experiment_id, s.agent_id, COALESCE(a.name, s.agent_id), s.role, s.quota_tier,
       s.hosted, s.launcher, s.flavor, s.hyperparameters, s.model, s.signed_up_at,
       s.instance_name, s.instance_restarts, s.instance_renewals, s.instance_stopped, s.instance_exited_at, s.instance_exit_log,
       s.instance_seen_at, s.instance_asked_at, s.instance_launch,
       s.instance_state, s.instance_progress_at, s.instance_end_reason, s.instance_activity, s.instance_activity_at,
       s.finished_at, s.finished_reason, s.added_by_kind, s.added_by_id,
       ` + domain.InstanceReleasedSQL(`LEAST(
           s.finished_at,
           (SELECT ended.closed_at FROM platform_experiments ended WHERE ended.id = s.platform_experiment_id),
           (SELECT cut.cut_at FROM platform_experiment_cuts cut WHERE cut.platform_experiment_id = s.platform_experiment_id AND cut.agent_id = s.agent_id),
           (SELECT planned.planner_ends_at FROM platform_experiments planned WHERE planned.id = s.platform_experiment_id AND s.role = '`+string(domain.SignupRolePlanner)+`'))`) + `,
       ` + runEndSQL

// scanAgentSignup reads agentSignupColumns, and after them whatever else the query selected.
func scanAgentSignup(row pgx.Row, more ...any) (domain.AgentSignup, error) {
	var signup domain.AgentSignup
	var hyperparameters []byte
	var instance domain.AgentInstance
	addedBy := domain.Actor{}
	var activity string
	var activityAt *time.Time
	if err := row.Scan(append([]any{&signup.PlatformExperimentID, &signup.AgentID, &signup.Name, &signup.Role, &signup.QuotaTier,
		&signup.Hosted, &signup.Launcher, &signup.Flavor, &hyperparameters, &signup.Model, &signup.SignedUpAt,
		&instance.Name, &instance.Restarts, &instance.Renewals, &instance.Stopped, &instance.ExitedAt, &instance.ExitLog,
		&instance.SeenAt, &instance.AskedAt, &instance.Launch, &instance.Reported, &instance.ProgressAt, &instance.EndReason, &activity, &activityAt,
		&signup.FinishedAt, &signup.FinishedReason, &addedBy.Kind, &addedBy.ID, &instance.Released, &signup.Ended}, more...)...); err != nil {
		return domain.AgentSignup{}, err
	}
	if addedBy.Kind == "" {
		addedBy.Kind = domain.ActorUnverified
	}
	signup.Actor = &addedBy
	if signup.Hosted {
		signup.Instance = &instance
		signup.Activity = domain.ActivityOf(activity, activityAt)
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
FROM experiment_signups s LEFT JOIN agents a ON a.id = s.agent_id
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

// LauncherAgent is a hosted agent as its launcher's desired state names it. Once its run Ended
// its machine is no longer wanted and is left to finish its last turn.
type LauncherAgent struct {
	domain.AgentSignup
	// AgentImage is the image the agent's platform experiment names for its machines, empty for
	// the platform's own.
	AgentImage string
	// EndsBy is the latest moment a machine rented for the agent now may still run
	// (launcherEndsBySQL).
	EndsBy time.Time
}

// launcherEndsBySQL is the latest moment platform experiment pe can still have a machine, $2
// seconds of wind-down included: of a draft the end of its planning, and once its planner
// answered the moment the planner is kept until, of an open one the end of the longest run it
// can still have, which starts at its baseline_due_at, and of a running one the end of its run.
// It moves when the owner changes starts_at or duration_hours, of an open one or a running one.
// The launcher moves the end of a machine rented before that to the new one where its provider
// can (provider.Extender), and one that cannot ends at the moment it was rented with.
const launcherEndsBySQL = `(CASE
            WHEN pe.status = 'closed' THEN pe.closed_at
            WHEN pe.status = 'draft' AND pe.planner_ends_at IS NOT NULL THEN pe.planner_ends_at
            WHEN pe.status = 'open' THEN pe.ends_at + make_interval(secs => pe.duration_hours * 3600)
            WHEN pe.status = 'running' AND pe.started_at IS NULL THEN ` + runOverAtSQL + `
            ELSE pe.ends_at END + make_interval(secs => $2))`

// launcherAgentEndsBySQL is launcherEndsBySQL for the agent s, but for the baseline agent of a
// draft, whose machine starts before the draft is accepted and has to outlast the baseline:
// it is bounded by the draft's own end, when an unaccepted proposal is closed.
const launcherAgentEndsBySQL = `(CASE WHEN pe.status = 'draft' AND s.role = '` + string(domain.SignupRoleBaseline) + `'
            THEN pe.ends_at + make_interval(secs => $2)
            ELSE ` + launcherEndsBySQL + ` END)`

// launcherAgentsFrom selects the hosted agents a launcher still has something to do for: those
// of a running platform experiment that were not cut and did not finish, those that measure the
// baseline of an open one, since no researcher's machine is rented before there is a baseline to
// start from, and those
// whose experiment closed, who were cut or who finished less than $2 seconds ago. A stopped agent is among them: it is asked for again
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
// the draft's planner_ends_at, for a revision asked for soon, and then let wind down for $2
// seconds, as it is after the draft closed. Once the draft is accepted it has nothing left to
// do, in an experiment that is then open or running.
const launcherAgentsFrom = `
FROM experiment_signups s
JOIN platform_experiments pe ON pe.id = s.platform_experiment_id
LEFT JOIN agents a ON a.id = s.agent_id
LEFT JOIN platform_experiment_cuts c ON c.platform_experiment_id = s.platform_experiment_id AND c.agent_id = s.agent_id
WHERE s.hosted AND s.launcher = $1 AND NOT (` + awaitsRepository + `)
  AND (pe.status = 'closed' AND pe.closed_at > now() - make_interval(secs => $2)
       OR s.role <> '` + string(domain.SignupRolePlanner) + `' AND (pe.status = 'running' OR pe.status = 'open' AND s.role = '` + string(domain.SignupRoleBaseline) + `')
       OR s.role = '` + string(domain.SignupRoleBaseline) + `' AND pe.status = 'draft' AND pe.accepts_at IS NOT NULL
       OR s.role = '` + string(domain.SignupRolePlanner) + `' AND pe.status = 'draft' AND (pe.planner_ends_at IS NULL OR pe.planner_ends_at > now() - make_interval(secs => $2)))
  AND (c.agent_id IS NULL OR c.cut_at > now() - make_interval(secs => $2))
  AND (s.finished_at IS NULL OR s.finished_at > now() - make_interval(secs => $2))`

// ListLauncherAgents returns the hosted agents the launcher's desired state is decided from.
func (s *PlatformExperimentsStore) ListLauncherAgents(ctx context.Context, launcher string, windDown time.Duration) ([]LauncherAgent, error) {
	rows, err := s.pool.pool.Query(ctx, `
SELECT `+agentSignupColumns+`, pe.agent_image, `+launcherAgentEndsBySQL+launcherAgentsFrom+`
ORDER BY s.platform_experiment_id, s.agent_id`, launcher, windDown.Seconds())
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.ListLauncherAgents: %w", err)
	}
	defer rows.Close()
	agents := []LauncherAgent{}
	for rows.Next() {
		var agent LauncherAgent
		var err error
		if agent.AgentSignup, err = scanAgentSignup(rows, &agent.AgentImage, &agent.EndsBy); err != nil {
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
