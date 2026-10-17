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

// CreateDraft creates a draft platform experiment with its planner, an agent of the draft's org
// signed up as the platform runs it, or creates neither. A draft takes no other signup: joining
// is for an open platform experiment (enrolTx), which a draft becomes when it is accepted.
//
// A draft names its repository, or the account the platform creates one on after this returns
// (RepositoriesToCreate).
func (s *Store) CreateDraft(ctx context.Context, pe *domain.PlatformExperiment, planner *domain.Agent, signup domain.AgentSignup) error {
	if pe.Status != domain.PlatformExpDraft || signup.AgentID != planner.ID || signup.Role != domain.SignupRolePlanner || !signup.InAgentSandbox {
		return fmt.Errorf("platform_experiments_store.CreateDraft: a draft is created with status draft and its sandboxed planner, got status %s and signup %+v", pe.Status, signup)
	}
	if (pe.Repository == "") == (pe.RepositoryAccount == "") {
		return fmt.Errorf("platform_experiments_store.CreateDraft: a draft names its repository or the account one is created on, got repository %q and account %q", pe.Repository, pe.RepositoryAccount)
	}
	return s.write(ctx, func(tx *Write) error {
		if err := tx.QueryRow(ctx, `SELECT org_id FROM projects WHERE id = $1`, pe.ProjectID).Scan(&planner.OrgID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrProjectNotFound
			}
			return fmt.Errorf("platform_experiments_store.CreateDraft: read project: %w", err)
		}
		if err := insertPlatformExperiment(ctx, tx, pe); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO agents (id, name, org_id, created_at)
VALUES ($1, $2, $3, $4)`, planner.ID, planner.Name, planner.OrgID, planner.CreatedAt); err != nil {
			return fmt.Errorf("platform_experiments_store.CreateDraft: create planner: %w", err)
		}
		model, hyperparameters, err := ownSettings(signup)
		if err != nil {
			return fmt.Errorf("platform_experiments_store.CreateDraft: %w", err)
		}
		var addedBy domain.Actor
		if signup.Actor != nil {
			addedBy = *signup.Actor
		}
		agentType, err := agentTypeTx(ctx, tx, pe.ID, signup.AgentType, domain.RoleOfSignup(signup.Role))
		if err != nil {
			return fmt.Errorf("platform_experiments_store.CreateDraft: %w", err)
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO experiment_signups (platform_experiment_id, agent_id, signed_up_at, role, quota_tier, in_agentsandbox, agent_type, hyperparameters, model, added_by_kind, added_by_id)
VALUES ($1, $2, NOW(), $3, COALESCE(NULLIF($4, ''), (SELECT default_quota_tier FROM platform_experiments WHERE id = $1)), $5, $6, $7, $8, $9, $10)`, pe.ID, signup.AgentID, string(signup.Role), string(signup.QuotaTier),
			signup.InAgentSandbox, agentType, hyperparameters, model, string(addedBy.Kind), addedBy.ID); err != nil {
			return fmt.Errorf("platform_experiments_store.CreateDraft: sign the planner up: %w", err)
		}
		tx.agentActivity(pe.ID, signup.AgentID, domain.ActivitySignedUp)
		return nil
	})
}

// ErrNotDraft is returned for a write that only a draft takes.
var ErrNotDraft = errors.New("platform experiment is not a draft")

// ErrPlanningOver is returned for a planner's answer to a draft whose planning limit has passed.
var ErrPlanningOver = errors.New("the draft's planning limit has passed")

// ErrGoalChanged is returned for a planner's answer to a goal the draft no longer has.
var ErrGoalChanged = errors.New("the draft's goal changed after the one its planner answered")

// answerDraftTx runs a planner's answer to goal goalVersion, which a draft takes until its ends_at
// while that is its goal. The sweep closes a draft past it within a minute; an answer in that
// minute is refused here. args are $2 on, and the goal version follows them.
func answerDraftTx(ctx context.Context, tx *Write, id string, goalVersion int, update string, args ...any) error {
	tag, err := tx.Exec(ctx, update+fmt.Sprintf(` WHERE id = $1 AND status = 'draft' AND ends_at > now() AND goal_version = $%d`, len(args)+2),
		append(append([]any{id}, args...), goalVersion)...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() > 0 {
		return nil
	}
	var draft bool
	var version int
	err = tx.QueryRow(ctx, `SELECT status = 'draft', goal_version FROM platform_experiments WHERE id = $1`, id).Scan(&draft, &version)
	switch {
	case errors.Is(err, pgx.ErrNoRows) || (err == nil && !draft):
		return ErrNotDraft
	case err != nil:
		return err
	case version != goalVersion:
		return ErrGoalChanged
	}
	return ErrPlanningOver
}

// plannerKeptSQL is until when the planner of a draft that is answered now keeps its machine: $4
// seconds from now, so a goal changed soon reaches the planner that is still there, and no
// later than the planning limit, which is the bound its machine was rented with. It reads ends_at
// as it was before the answer moved it, so an answer that replaces one leaves the moment alone.
const plannerKeptSQL = `CASE WHEN proposed_at IS NULL THEN LEAST(now() + make_interval(secs => $4), ends_at) ELSE planner_ends_at END`

// Proposed is what a planner's proposal writes beside the proposal itself.
type Proposed struct {
	// WaitUntil is when the draft is closed if nobody takes the proposal up.
	WaitUntil time.Time
	// KeepPlanner is how long the planner's machine is kept after the proposal (plannerKeptSQL).
	KeepPlanner time.Duration
	// AcceptsAt is when the platform accepts the proposal as proposed, nil for never.
	AcceptsAt *time.Time
	// Baseline is the agent that is to measure the baseline, signed up sandboxed as BaselineSignup
	// says when the draft has no agent of role baseline yet. Its machine starts while AcceptsAt
	// is set (launcherAgentsFrom), so that it is up when the proposal is accepted. nil signs up
	// nobody.
	Baseline       *domain.Agent
	BaselineSignup domain.AgentSignup
}

// SetProposal stores what a draft's planner proposed for goal goalVersion, marks it proposed now,
// and moves the draft's end to p.WaitUntil: from then on it waits for a person, or for
// p.AcceptsAt, and no longer for the planner, whose machine is kept for p.KeepPlanner more
// (plannerKeptSQL). The draft's baseline agent is signed up in the same write, once.
// ErrNotDraft, ErrPlanningOver, and ErrGoalChanged once the draft has another goal.
func (s *Store) SetProposal(ctx context.Context, id string, goalVersion int, proposal []byte, p Proposed) error {
	if p.Baseline != nil && (p.BaselineSignup.AgentID != p.Baseline.ID || p.BaselineSignup.Role != domain.SignupRoleBaseline || !p.BaselineSignup.InAgentSandbox) {
		return fmt.Errorf("platform_experiments_store.SetProposal: a proposal signs up a sandboxed agent of role baseline, got %+v", p.BaselineSignup)
	}
	return s.write(ctx, func(tx *Write) error {
		err := answerDraftTx(ctx, tx, id, goalVersion, `
UPDATE platform_experiments SET proposal = $2, proposed_at = now(), declined_reason = '', planner_ends_at = `+plannerKeptSQL+`, ends_at = $3, accepts_at = $5, updated_at = now()`,
			proposal, p.WaitUntil, p.KeepPlanner.Seconds(), p.AcceptsAt)
		if errors.Is(err, ErrNotDraft) || errors.Is(err, ErrPlanningOver) || errors.Is(err, ErrGoalChanged) {
			return err
		}
		if err != nil {
			return fmt.Errorf("platform_experiments_store.SetProposal: %w", err)
		}
		tx.runChange(id, domain.EventPlatformExperimentProposal, "proposed", "")
		if p.Baseline != nil {
			if err := signUpDraftBaseline(ctx, tx, id, p.Baseline, p.BaselineSignup); err != nil {
				return fmt.Errorf("platform_experiments_store.SetProposal: %w", err)
			}
		}
		return nil
	})
}

// signUpDraftBaseline creates agent in the draft's org and signs it up as signup says, unless the
// draft has an agent of role baseline already. Its quota is written when the draft is accepted,
// from the budget the accept states (AcceptDraft).
func signUpDraftBaseline(ctx context.Context, tx *Write, id string, agent *domain.Agent, signup domain.AgentSignup) error {
	var has bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM experiment_signups WHERE platform_experiment_id = $1 AND role = $2)`,
		id, string(domain.SignupRoleBaseline)).Scan(&has); err != nil {
		return fmt.Errorf("find the baseline agent: %w", err)
	}
	if has {
		return nil
	}
	names, err := agentNamesTx(ctx, tx, id)
	if err != nil {
		return err
	}
	if agent.Name == "" {
		agent.Name = domain.AgentName(signup.Role, func(name string) bool { _, taken := names[name]; return taken })
	}
	if err := tx.QueryRow(ctx, `SELECT p.org_id FROM platform_experiments pe JOIN projects p ON p.id = pe.project_id WHERE pe.id = $1`, id).Scan(&agent.OrgID); err != nil {
		return fmt.Errorf("read the draft's org: %w", err)
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO agents (id, name, org_id, created_at)
VALUES ($1, $2, $3, $4)`, agent.ID, agent.Name, agent.OrgID, agent.CreatedAt); err != nil {
		return fmt.Errorf("create the baseline agent: %w", err)
	}
	model, hyperparameters, err := ownSettings(signup)
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	var addedBy domain.Actor
	if signup.Actor != nil {
		addedBy = *signup.Actor
	}
	agentType, err := agentTypeTx(ctx, tx, id, signup.AgentType, domain.RoleOfSignup(signup.Role))
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO experiment_signups (platform_experiment_id, agent_id, signed_up_at, role, quota_tier, in_agentsandbox, agent_type, hyperparameters, model, added_by_kind, added_by_id)
VALUES ($1, $2, NOW(), $3, COALESCE(NULLIF($4, ''), (SELECT default_quota_tier FROM platform_experiments WHERE id = $1)), $5, $6, $7, $8, $9, $10)`, id, signup.AgentID, string(signup.Role), string(signup.QuotaTier),
		signup.InAgentSandbox, agentType, hyperparameters, model, string(addedBy.Kind), addedBy.ID); err != nil {
		return fmt.Errorf("sign the baseline agent up: %w", err)
	}
	tx.agentActivity(id, signup.AgentID, domain.ActivitySignedUp)
	return nil
}

// ErrNotProposed is returned for holding the acceptance of a draft that has no proposal standing.
var ErrNotProposed = errors.New("the draft has no proposal standing")

// HoldAccept stops the platform from accepting the proposed draft id by itself: a person edits
// the proposal and accepts it, changes its goal or closes it. Its baseline agent's machine is released
// with it, and is asked for again once the draft is accepted. Holding one held changes nothing.
// ErrNotDraft, and ErrNotProposed for a draft that has no proposal standing.
func (s *Store) HoldAccept(ctx context.Context, id string) error {
	return s.write(ctx, func(tx *Write) error {
		var err error
		var status string
		var proposed bool
		err = tx.QueryRow(ctx, `SELECT status, proposed_at IS NOT NULL AND proposal IS NOT NULL FROM platform_experiments WHERE id = $1 FOR UPDATE`, id).Scan(&status, &proposed)
		switch {
		case errors.Is(err, pgx.ErrNoRows) || (err == nil && status != string(domain.PlatformExpDraft)):
			return ErrNotDraft
		case err != nil:
			return fmt.Errorf("platform_experiments_store.HoldAccept: lock draft: %w", err)
		case !proposed:
			return ErrNotProposed
		}
		if _, err := tx.Exec(ctx, `UPDATE platform_experiments SET accepts_at = NULL, updated_at = now() WHERE id = $1 AND accepts_at IS NOT NULL`, id); err != nil {
			return fmt.Errorf("platform_experiments_store.HoldAccept: %w", err)
		}
		return nil
	})
}

// DraftsToAccept is the proposed drafts whose accepts_at has passed, oldest first.
func (s *Store) DraftsToAccept(ctx context.Context) ([]string, error) {
	rows, err := s.pool.pool.Query(ctx, `
SELECT id FROM platform_experiments
WHERE accepts_at <= now() AND status = 'draft' AND proposed_at IS NOT NULL AND proposal IS NOT NULL
ORDER BY accepts_at`)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.DraftsToAccept: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.DraftsToAccept: %w", err)
	}
	return ids, nil
}

// Decline stores why a draft's planner proposes nothing for goal goalVersion. It answers the goal
// as a proposal does: the draft waits for a person until waitUntil, when the platform closes it as
// it closes a proposal nobody took up, and the planner's machine is kept for keepPlanner more, as
// after a proposal. A proposal made before no longer stands, so it is cleared and there is nothing
// to accept. proposed_at is the column of the planner's answer, whichever it was: with no proposal
// beside it the draft is declined. The errors of SetProposal.
func (s *Store) Decline(ctx context.Context, id string, goalVersion int, reason string, waitUntil time.Time, keepPlanner time.Duration) error {
	return s.write(ctx, func(tx *Write) error {
		err := answerDraftTx(ctx, tx, id, goalVersion, `
UPDATE platform_experiments SET declined_reason = $2, proposal = NULL, proposed_at = now(), planner_ends_at = `+plannerKeptSQL+`, ends_at = $3, accepts_at = NULL, updated_at = now()`,
			reason, waitUntil, keepPlanner.Seconds())
		if errors.Is(err, ErrNotDraft) || errors.Is(err, ErrPlanningOver) || errors.Is(err, ErrGoalChanged) {
			return err
		}
		if err != nil {
			return fmt.Errorf("platform_experiments_store.Decline: %w", err)
		}
		tx.runChange(id, domain.EventPlatformExperimentProposal, "declined", "")
		return nil
	})
}

// ErrNothingProposed is returned for accepting a draft whose planner has proposed nothing or
// declined, or whose goal changed since its proposal.
var ErrNothingProposed = errors.New("nothing is proposed")

// AcceptDraft makes a proposed draft the open platform experiment pe describes and signs its
// researchers up, and its baseline agent where its proposal signed none up, or does neither. pe carries what the proposal stated and what the person who
// accepts stated, and who accepted it, when and with which answers, beside the proposal kept as proposed; its baseline is measured from its StartsAt, and it is open until its
// BaselineDueAt. Its planner has nothing left to answer, so the time its machine was kept for a
// change of the goal ends with the accept. The limits a signup holds are held here too
// (ErrMaxAgentsReached, ErrMaxAgentSandboxesReached).
func (s *Store) AcceptDraft(ctx context.Context, pe *domain.PlatformExperiment, create []*domain.Agent, signups []domain.AgentSignup) error {
	if pe.Coordinator == nil || !pe.CoordinatorInAgentSandbox {
		return errors.New("platform_experiments_store.AcceptDraft: an accepted draft is run by a coordinator the platform hosts")
	}
	metrics, err := json.Marshal(pe.Metrics)
	if err != nil {
		return fmt.Errorf("platform_experiments_store.AcceptDraft: marshal metrics: %w", err)
	}
	stages, err := json.Marshal(pe.Stages)
	if err != nil {
		return fmt.Errorf("platform_experiments_store.AcceptDraft: marshal stages: %w", err)
	}
	researchers, err := poolJSON(pe.Researchers)
	if err != nil {
		return fmt.Errorf("platform_experiments_store.AcceptDraft: marshal researchers: %w", err)
	}
	if pe.AcceptedAt == nil {
		return errors.New("platform_experiments_store.AcceptDraft: an accept states when it was made")
	}
	var answers []byte
	if len(pe.AcceptedAnswers) > 0 {
		if answers, err = json.Marshal(pe.AcceptedAnswers); err != nil {
			return fmt.Errorf("platform_experiments_store.AcceptDraft: marshal answers: %w", err)
		}
	}
	var acceptedBy domain.Actor
	if pe.AcceptedBy != nil {
		acceptedBy = *pe.AcceptedBy
	}
	return s.write(ctx, func(tx *Write) error {
		var status, description string
		var proposed bool
		var durationHours float64
		err = tx.QueryRow(ctx, `SELECT status, proposed_at IS NOT NULL AND proposal IS NOT NULL, description, duration_hours
FROM platform_experiments WHERE id = $1 FOR UPDATE`, pe.ID).Scan(&status, &proposed, &description, &durationHours)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && status != string(domain.PlatformExpDraft)) {
			return ErrNotDraft
		}
		if err != nil {
			return fmt.Errorf("platform_experiments_store.AcceptDraft: lock draft: %w", err)
		}
		if !proposed {
			return ErrNothingProposed
		}
		if _, err := moveRun(ctx, tx, `
UPDATE platform_experiments
SET status = 'open', `+statusMoved+`, name = $2, description = $3, budget_accelerator_hours = $4, max_agents = $5, metrics = $6,
    report_interval_seconds = $7, starts_at = $14, ends_at = $8, stages = $9, current_stage = 1,
    default_quota_tier = $10, max_concurrent_accelerators = $11,
    coordinator_in_agentsandbox = true, coordinator_model = NULLIF($12, ''), duration_hours = $13, updated_at = now(),
    planner_ends_at = LEAST(planner_ends_at, now()), accepts_at = NULL,
    accepted_at = $15, accepted_by_kind = $16, accepted_by_id = $17, accepted_answers = $18, researchers = $19
WHERE id = $1`, pe.ID, pe.Name, pe.Description, pe.BudgetAcceleratorHours, pe.MaxAgents, metrics,
			pe.ReportIntervalSeconds, pe.BaselineDueAt, stages,
			string(pe.DefaultQuotaTier), pe.MaxConcurrentAccelerators, pe.Coordinator.Model, pe.DurationHours, optionalTime(pe.StartsAt),
			pe.AcceptedAt, string(acceptedBy.Kind), acceptedBy.ID, answers, researchers); err != nil {
			return fmt.Errorf("platform_experiments_store.AcceptDraft: %w", err)
		}
		if pe.Description != description || pe.DurationHours != durationHours {
			tx.runChange(pe.ID, domain.EventPlatformExperimentDescription, "", "")
		}
		if err := poolTypesTx(ctx, tx, pe.ID, pe.Researchers); err != nil {
			return fmt.Errorf("platform_experiments_store.AcceptDraft: %w", err)
		}
		if _, err := enrolTx(ctx, tx, pe.ID, create, signups, nil); err != nil {
			return err
		}
		// The baseline agent its proposal signed up gets its quota from the budget stated here.
		if err := reallocateBaselineQuotas(ctx, tx, pe); err != nil {
			return fmt.Errorf("platform_experiments_store.AcceptDraft: %w", err)
		}
		return nil
	})
}

// AgentStanding reads the standing of agentID in platformExpID.
func (s *Store) AgentStanding(ctx context.Context, platformExpID, agentID string) (domain.AgentStanding, error) {
	t := &domain.Tenancy{PlatformExperimentID: platformExpID}
	var out domain.AgentStanding
	err := s.pool.pool.QueryRow(ctx, `
SELECT pe.project_id, p.org_id, pe.shared, ARRAY(`+priorRuns+`), pe.closed_at,
       s.agent_id IS NOT NULL,
       (SELECT cut.cut_at FROM platform_experiment_cuts cut WHERE cut.platform_experiment_id = pe.id AND cut.agent_id = $2),
       CASE WHEN s.role = '`+string(domain.SignupRolePlanner)+`' AND pe.planner_ends_at <= now() THEN pe.planner_ends_at END,
       s.finished_at
FROM platform_experiments pe JOIN projects p ON p.id = pe.project_id
LEFT JOIN experiment_signups s ON s.platform_experiment_id = pe.id AND s.agent_id = $2
WHERE pe.id = $1 AND pe.removed_at IS NULL`, platformExpID, agentID).Scan(&t.ProjectID, &t.OrgID, &t.Shared, &t.PriorRuns, &out.ClosedAt,
		&out.SignedUp, &out.CutAt, &out.PlannerAnsweredAt, &out.FinishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.AgentStanding{}, nil
	}
	if err != nil {
		return domain.AgentStanding{}, fmt.Errorf("platform_experiments_store.AgentStanding: %w", err)
	}
	out.Tenancy = t
	return out, nil
}

// JobHasFinding says a finding was filed for the job.
func (s *Store) JobHasFinding(ctx context.Context, experimentID string) (bool, error) {
	var found bool
	if err := s.pool.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM hypothesis_findings WHERE experiment_id = $1)`, experimentID).Scan(&found); err != nil {
		return false, fmt.Errorf("platform_experiments_store.JobHasFinding: %w", err)
	}
	return found, nil
}

// ErrNotEditable is returned for a change of the goal of a platform experiment that is closed or
// was created whole, with no goal.
var ErrNotEditable = errors.New("the platform experiment has no goal that changes")

// SetGoal stores goal as what the platform experiment id answers, one goal_version on, and tells
// whoever acts on it. A draft is given back to its planner: what it answered stays to be read,
// proposed_at, accepts_at and planner_ends_at are cleared, so nothing is accepted by itself and the
// launcher wants the planner again, and the draft ends at planUntil unless the planner answers. An
// open or running one tells its coordinator, which brings the brief in line. ErrNotEditable.
func (s *Store) SetGoal(ctx context.Context, id, goal string, planUntil time.Time) error {
	return s.write(ctx, func(tx *Write) error {
		var status string
		var version int
		var planner *string
		err := tx.QueryRow(ctx, `
UPDATE platform_experiments SET goal = $2, goal_version = goal_version + 1, updated_at = now(),
  proposed_at = CASE WHEN status = 'draft' THEN NULL ELSE proposed_at END,
  accepts_at = CASE WHEN status = 'draft' THEN NULL ELSE accepts_at END,
  planner_ends_at = CASE WHEN status = 'draft' THEN NULL ELSE planner_ends_at END,
  ends_at = CASE WHEN status = 'draft' THEN $3 ELSE ends_at END
WHERE id = $1 AND status <> 'closed' AND goal <> '' AND removed_at IS NULL
RETURNING status, goal_version, (SELECT agent_id FROM experiment_signups WHERE platform_experiment_id = $1 AND role = $4)`,
			id, goal, planUntil, string(domain.SignupRolePlanner)).Scan(&status, &version, &planner)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotEditable
		}
		if err != nil {
			return fmt.Errorf("platform_experiments_store.SetGoal: %w", err)
		}
		recipient := domain.CoordinatorRecipient
		if status == string(domain.PlatformExpDraft) {
			if planner == nil {
				return fmt.Errorf("platform_experiments_store.SetGoal: draft %s has no planner", id)
			}
			recipient = *planner
			// A planner whose machine ended because its work was done is asked for under a new
			// launch, with its own restarts: a launch the launcher's reconcile does not know spends
			// none of the old one's. One whose machine is still there keeps it and its launch.
			if _, err := tx.Exec(ctx, `UPDATE agentsandbox_instances SET launch = ''
WHERE platform_experiment_id = $1 AND agent_id = $2 AND name = ''`, id, recipient); err != nil {
				return fmt.Errorf("platform_experiments_store.SetGoal: renew the planner's launch: %w", err)
			}
		}
		tx.Change(domain.Change{Kind: domain.EventPlatformExperimentGoal, Subject: id, Value: strconv.Itoa(version),
			PlatformExperimentID: id, AgentID: recipient})
		return nil
	})
}
