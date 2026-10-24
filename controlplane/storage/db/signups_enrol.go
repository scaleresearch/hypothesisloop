package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/scaleresearch/hypothesisloop/contract/platformapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// ---- experiment_signups ----

// ErrMaxAgentsReached is an executor's signup refused because max_agents executors are at work.
var ErrMaxAgentsReached = errors.New("max_agents reached")

// ErrMaxAgentSandboxesReached is returned when a platform experiment would have the platform run more
// machines than its max_agentsandboxes, or leave none of them to measure its baseline.
var ErrMaxAgentSandboxesReached = errors.New("max_agentsandboxes reached")

// ErrAgentNameTaken is an agent refused a place because another agent of the platform experiment
// carries its name.
var ErrAgentNameTaken = errors.New("another agent of the platform experiment has this name")

// ErrSignupClosed is a signup or a removal refused because the platform experiment is no longer
// open, or does not exist. Once it runs only an executor or a researcher joins it (JoinRunning).
var ErrSignupClosed = errors.New("platform experiment is not open")

// ErrNoShareLeft is an executor refused a place in a running platform experiment because its
// even part of what the executors left unspent would not hold one trial (LateJoin.MinShare).
var ErrNoShareLeft = errors.New("no unspent share left to give an executor")

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
func (s *Store) Signup(ctx context.Context, platformExpID string, signup domain.AgentSignup) (bool, error) {
	inserted, err := s.enrol(ctx, platformExpID, nil, []domain.AgentSignup{signup}, nil)
	if errors.Is(err, ErrSignupClosed) {
		return false, nil
	}
	return inserted == 1, err
}

// SpawnAgent creates an agent in the platform experiment's org and signs it up, both or neither.
// An agent without a name is given one (domain.AgentName), which agent then carries.
// ErrAgentNameTaken when its name is another agent's in the experiment. ErrSignupClosed when the experiment is not open, ErrMaxAgentsReached when it is a competitor
// with no place left, ErrMaxAgentSandboxesReached when it is one machine too many.
func (s *Store) SpawnAgent(ctx context.Context, platformExpID string, agent *domain.Agent, signup domain.AgentSignup) error {
	_, err := s.enrol(ctx, platformExpID, []*domain.Agent{agent}, []domain.AgentSignup{signup}, nil)
	return err
}

// LateJoin is what an agent that joins a running platform experiment is given its quota from,
// an executor's (domain.LateShare), and whether the platform adds it to keep the pool.
type LateJoin struct {
	// Used is what each executor of the platform experiment has used of its quota, the jobs
	// that have not ended at their estimate.
	Used map[string]float64
	// BurstFraction sizes the newcomer's burst hours from its share, as at the start.
	BurstFraction float64
	// MinShare is the least share worth giving: what one trial of the baseline holds.
	MinShare float64
	// Pool is an agent the platform adds to keep the pool (quota.KeepAgents): its entry
	// must want one beyond those at work when the row is locked (poolWantsTx).
	Pool bool
}

// ErrPoolSatisfied refuses an agent the platform would add to keep the pool when, with the
// platform experiment locked, its entry wants none beyond those at work, or it would replace
// another beyond domain.MaxReplacementsAnHour of its entry.
var ErrPoolSatisfied = errors.New("the pool entry wants no agent added now")

// poolWantsTx refuses signup, an agent the platform adds to keep the pool of the platform
// experiment platformExpID whose row the transaction locked, with ErrPoolSatisfied: what the
// keeper decided from is checked again where no other write can move it.
func poolWantsTx(ctx context.Context, tx *Write, platformExpID string, signup domain.AgentSignup) error {
	var pool domain.AgentPool
	if err := tx.QueryRow(ctx, `SELECT agents FROM platform_experiments WHERE id = $1`, platformExpID).Scan(&pool); err != nil {
		return fmt.Errorf("read agents: %w", err)
	}
	atWork, err := agentsAtWork(ctx, tx, platformExpID)
	if err != nil {
		return err
	}
	key := domain.PoolKey{AgentType: signup.AgentType, Model: signup.Model}
	i := slices.IndexFunc(pool, func(e domain.AgentPoolEntry) bool { return e.Key() == key })
	if i < 0 || pool[i].Count <= atWork[key] {
		return ErrPoolSatisfied
	}
	if signup.Replaces == "" {
		return nil
	}
	recent, err := recentReplacements(ctx, tx, platformExpID)
	if err != nil {
		return err
	}
	if recent[key] >= domain.MaxReplacementsAnHour {
		return ErrPoolSatisfied
	}
	return nil
}

// JoinRunning gives an executor or a researcher a place in a running platform experiment, and an
// executor its quota: a fair part of what the executors left unspent, taken first from the ones that ended (domain.LateShare).
// agent is created first when not nil. The errors are SpawnAgent's, ErrSignupClosed for any
// other role or a platform experiment that is not running, and ErrNoShareLeft.
func (s *Store) JoinRunning(ctx context.Context, platformExpID string, agent *domain.Agent, signup domain.AgentSignup, late LateJoin) error {
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
func (s *Store) enrol(ctx context.Context, platformExpID string, create []*domain.Agent, signups []domain.AgentSignup, late *LateJoin) (int, error) {
	var inserted int
	err := s.write(ctx, func(tx *Write) error {
		var err error
		inserted, err = enrolTx(ctx, tx, platformExpID, create, signups, late)
		return err
	})
	if err != nil {
		return 0, err
	}
	return inserted, nil
}

// allocateBaselineQuota writes what an agent of role baseline holds while its platform experiment
// is open: half the budget (domain.BaselineShare), in the column its tier names
// (domain.ApplyQuotaTier). It is written when the agent joins and again whenever the budget of
// the open experiment changes.
func allocateBaselineQuota(ctx context.Context, tx *Write, platformExpID, agentID string, tier domain.QuotaTier, budget float64) error {
	guaranteed, burst := domain.ApplyQuotaTier(tier, domain.BaselineShare(budget), 0)
	id, err := domain.NewID(domain.AgentQuotaIDPrefix)
	if err != nil {
		return err
	}
	if err := upsertAgentQuota(ctx, tx, &domain.AgentQuota{
		ID: id, AgentID: agentID, PlatformExperimentID: platformExpID,
		GuaranteedAcceleratorHours: guaranteed, BurstAcceleratorHours: burst, CreatedAt: time.Now().UTC(),
	}); err != nil {
		return fmt.Errorf("allocate the quota of baseline agent %s: %w", agentID, err)
	}
	return nil
}

// enrolTx is enrol inside the caller's transaction. With late it adds executors and researchers
// to a running platform experiment, and to nothing else.
//
// An agent of role baseline is given its quota here, with its place: its job runs while the
// platform experiment is open, before the start divides the budget (allocateBaselineQuota). So
// is an executor that joins a running one (allocateLateQuota).
func enrolTx(ctx context.Context, tx *Write, platformExpID string, create []*domain.Agent, signups []domain.AgentSignup, late *LateJoin) (int, error) {
	var status, orgID string
	var maxAgents, maxAgentSandboxes int
	var coordinatorInAgentSandbox bool
	var budget float64
	var defaultTier domain.QuotaTier
	if err := tx.QueryRow(ctx, `
SELECT pe.status, pe.max_agents, pe.max_agentsandboxes, pe.coordinator_in_agentsandbox, `+orgOfSQL("pe.id")+`, pe.budget_accelerator_hours, pe.default_quota_tier
FROM platform_experiments pe
WHERE pe.id=$1 FOR UPDATE OF pe`, platformExpID).Scan(&status, &maxAgents, &maxAgentSandboxes, &coordinatorInAgentSandbox, &orgID, &budget, &defaultTier); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, ErrSignupClosed
		}
		return 0, fmt.Errorf("platform_experiments_store.Signup: lock experiment: %w", err)
	}
	// A late join is to a running platform experiment, any other to an open one.
	running, joins := late != nil, domain.PlatformExpOpen
	if running {
		joins = domain.PlatformExpRunning
	}
	if status != string(joins) || slices.ContainsFunc(signups, func(signup domain.AgentSignup) bool { return !domain.TakesSignup(joins, signup.Role) }) {
		return 0, ErrSignupClosed
	}
	if running && late.Pool {
		for _, signup := range signups {
			if err := poolWantsTx(ctx, tx, platformExpID, signup); err != nil {
				return 0, err
			}
		}
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
INSERT INTO agents (id, name, org_id, created_at)
VALUES ($1, $2, $3, $4)`, agent.ID, agent.Name, agent.OrgID, agent.CreatedAt); err != nil {
			return 0, fmt.Errorf("platform_experiments_store.Signup: create agent %s: %w", agent.ID, err)
		}
	}
	created := make(map[string]bool, len(create))
	for _, agent := range create {
		created[agent.ID] = true
	}
	inserted, competitors, sandboxed := 0, false, false
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
		model, hyperparameters, err := ownSettings(signup)
		if err != nil {
			return 0, fmt.Errorf("platform_experiments_store.Signup: %w", err)
		}
		var addedBy domain.Actor
		if signup.Actor != nil {
			addedBy = *signup.Actor
		}
		agentType, err := agentTypeTx(ctx, tx, platformExpID, signup.AgentType, domain.RoleOfSignup(signup.Role))
		if err != nil {
			return 0, err
		}
		signup.QuotaTier = domain.ResolveQuotaTier(signup.QuotaTier, defaultTier)
		var why string
		err = tx.QueryRow(ctx, `
INSERT INTO experiment_signups (platform_experiment_id, agent_id, signed_up_at, role, quota_tier, in_agentsandbox, agent_type, hyperparameters, model, added_by_kind, added_by_id,
                                replaces)
VALUES ($1, $2, NOW(), $3, $4, $5, $6, $7, $8, $9, $10, $11::text)
ON CONFLICT (platform_experiment_id, agent_id) DO NOTHING
RETURNING COALESCE((SELECT CASE WHEN gone.finished_at IS NOT NULL THEN 'it finished: ' || gone.finished_reason
                                ELSE 'its machine stopped with no restart left' END
                    FROM experiment_signups gone WHERE gone.platform_experiment_id = $1 AND gone.agent_id = $11::text), '')`,
			platformExpID, signup.AgentID, string(signup.Role), string(signup.QuotaTier),
			signup.InAgentSandbox, agentType, hyperparameters, model, string(addedBy.Kind), addedBy.ID, signup.Replaces).Scan(&why)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("platform_experiments_store.Signup: %w", err)
		}
		tx.agentActivity(platformExpID, signup.AgentID, domain.ActivitySignedUp)
		if signup.Replaces != "" {
			tx.Change(domain.Change{Kind: platformapi.EventAgentReplaced, Subject: signup.AgentID, Value: signup.Replaces, Detail: why,
				PlatformExperimentID: platformExpID, AgentID: signup.AgentID})
		}
		inserted++
		competitors = competitors || signup.Role == domain.SignupRoleCompetitor
		sandboxed = sandboxed || signup.InAgentSandbox
		if running {
			if signup.SharesBudget() {
				joined = append(joined, signup)
			}
			continue
		}
		if signup.Role != domain.SignupRoleBaseline {
			continue
		}
		if err := allocateBaselineQuota(ctx, tx, platformExpID, signup.AgentID, signup.QuotaTier, budget); err != nil {
			return 0, fmt.Errorf("platform_experiments_store.Signup: %w", err)
		}
	}
	// max_agentsandboxes bounds the machines the platform rents for the experiment at once. An agent that
	// finished holds none, nor does one whose machine stopped with no restart left, and a planner
	// is no agent of an open experiment. One machine is kept for measuring the baseline, which
	// nothing starts without: the coordinator and the agents that do not measure it take the
	// others. A baseline agent whose machine stopped leaves that one to the next.
	if sandboxed || (len(signups) == 0 && coordinatorInAgentSandbox) {
		var measuring, others int
		if err := tx.QueryRow(ctx, `
SELECT COUNT(*) FILTER (WHERE role = $2), COUNT(*) FILTER (WHERE role <> $2)
FROM experiment_signups s
WHERE s.platform_experiment_id = $1 AND s.in_agentsandbox AND s.finished_at IS NULL AND NOT `+stoppedSQL+` AND s.role <> $3`,
			platformExpID, string(domain.SignupRoleBaseline), string(domain.SignupRolePlanner)).Scan(&measuring, &others); err != nil {
			return 0, fmt.Errorf("platform_experiments_store.Signup: count sandboxed: %w", err)
		}
		if coordinatorInAgentSandbox {
			others++
		}
		if others > maxAgentSandboxes-1 || measuring+others > maxAgentSandboxes {
			return 0, ErrMaxAgentSandboxesReached
		}
	}
	if inserted == 0 {
		return 0, nil
	}
	// max_agents bounds the executors at work at once (executorAtWorkSQL), and nobody of another
	// role takes a place.
	if competitors {
		var count int
		if err := tx.QueryRow(ctx, `
SELECT COUNT(*) FROM experiment_signups s
WHERE s.platform_experiment_id = $1 AND `+executorAtWorkSQL, platformExpID).Scan(&count); err != nil {
			return 0, fmt.Errorf("platform_experiments_store.Signup: count competitors: %w", err)
		}
		if count > maxAgents {
			return 0, ErrMaxAgentsReached
		}
	}
	for _, signup := range joined {
		if err := allocateLateQuota(ctx, tx, platformExpID, signup, *late); err != nil {
			return 0, err
		}
	}
	return inserted, nil
}

// allocateLateQuota gives signup, an executor joining the running platform experiment, its share
// of what the executors left unspent (domain.LateShare), and takes that share from the quotas of
// the others, never below it nor below what they used. The experiment row is locked, so two executors joining at once do not
// take the same hours.
func allocateLateQuota(ctx context.Context, tx *Write, platformExpID string, signup domain.AgentSignup, late LateJoin) error {
	rows, err := tx.Query(ctx, `
SELECT q.agent_id, q.guaranteed_accelerator_hours, q.burst_accelerator_hours,
       s.finished_at IS NOT NULL OR `+stoppedSQL+`
FROM agent_quotas q
JOIN experiment_signups s ON s.platform_experiment_id = q.platform_experiment_id AND s.agent_id = q.agent_id
WHERE q.platform_experiment_id = $1 AND s.role = $2 AND s.agent_id <> $3
  AND NOT EXISTS (SELECT 1 FROM platform_experiment_cuts c WHERE c.platform_experiment_id = q.platform_experiment_id AND c.agent_id = q.agent_id)
ORDER BY s.signed_up_at, s.agent_id`, platformExpID, string(domain.SignupRoleCompetitor), signup.AgentID)
	if err != nil {
		return fmt.Errorf("platform_experiments_store.JoinRunning: read quotas: %w", err)
	}
	var quotas []domain.ExecutorQuota
	for rows.Next() {
		var q domain.ExecutorQuota
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
		if err := writeQuotas(ctx, tx, `
UPDATE agent_quotas SET
    guaranteed_accelerator_hours = CASE WHEN guaranteed_accelerator_hours > 0 THEN GREATEST(0, guaranteed_accelerator_hours - $3) ELSE 0 END,
    burst_accelerator_hours = CASE WHEN guaranteed_accelerator_hours > 0 THEN burst_accelerator_hours ELSE GREATEST(0, burst_accelerator_hours - $3) END,
    updated_at = NOW()
WHERE platform_experiment_id = $1 AND agent_id = $2`, platformExpID, agentID, take); err != nil {
			return fmt.Errorf("platform_experiments_store.JoinRunning: take from %s: %w", agentID, err)
		}
	}
	guaranteed, burst := domain.ApplyQuotaTier(signup.QuotaTier, share, domain.RoundHours(share*late.BurstFraction))
	id, err := domain.NewID(domain.AgentQuotaIDPrefix)
	if err != nil {
		return err
	}
	if err := upsertAgentQuota(ctx, tx, &domain.AgentQuota{
		ID: id, AgentID: signup.AgentID, PlatformExperimentID: platformExpID,
		GuaranteedAcceleratorHours: guaranteed, BurstAcceleratorHours: burst, CreatedAt: time.Now().UTC(),
	}); err != nil {
		return fmt.Errorf("platform_experiments_store.JoinRunning: allocate the quota of %s: %w", signup.AgentID, err)
	}
	return nil
}

// AgentNames returns the names carried in a platform experiment, each with its agent: of the
// agents signed up to it and of those that have a job in it.
func (s *Store) AgentNames(ctx context.Context, platformExpID string) (map[string]string, error) {
	names, err := agentNamesTx(ctx, s.pool.pool, platformExpID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.AgentNames: %w", err)
	}
	return names, nil
}

func agentNamesTx(ctx context.Context, tx interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}, platformExpID string) (map[string]string, error) {
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

// ownSettings is what a signup row holds of signup's model and hyperparameters: NULL for each it
// leaves to its agent type.
func ownSettings(signup domain.AgentSignup) (model, hyperparameters any, err error) {
	if signup.Model != "" {
		model = signup.Model
	}
	if signup.Hyperparameters != nil {
		encoded, err := json.Marshal(signup.Hyperparameters)
		if err != nil {
			return nil, nil, fmt.Errorf("encode hyperparameters: %w", err)
		}
		hyperparameters = string(encoded)
	}
	return model, hyperparameters, nil
}
