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

// CreateDraft creates a draft platform experiment with its planner, an agent of the draft's org
// signed up as the platform runs it, or creates neither. A draft takes no other signup: joining
// is for an open platform experiment (enrolTx), which a draft becomes when it is accepted.
//
// A draft names its repository, or the account the platform creates one on after this returns
// (RepositoriesToCreate).
func (s *PlatformExperimentsStore) CreateDraft(ctx context.Context, pe *domain.PlatformExperiment, planner *domain.Agent, signup domain.AgentSignup) error {
	if pe.Status != domain.PlatformExpDraft || signup.AgentID != planner.ID || signup.Role != domain.SignupRolePlanner || !signup.Hosted {
		return fmt.Errorf("platform_experiments_store.CreateDraft: a draft is created with status draft and its hosted planner, got status %s and signup %+v", pe.Status, signup)
	}
	if (pe.Repository == "") == (pe.RepositoryAccount == "") {
		return fmt.Errorf("platform_experiments_store.CreateDraft: a draft names its repository or the account one is created on, got repository %q and account %q", pe.Repository, pe.RepositoryAccount)
	}
	tx, err := s.pool.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("platform_experiments_store.CreateDraft: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
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
INSERT INTO agents (id, name, org_id, performance_score, created_at)
VALUES ($1, $2, $3, $4, $5)`, planner.ID, planner.Name, planner.OrgID, planner.PerformanceScore, planner.CreatedAt); err != nil {
		return fmt.Errorf("platform_experiments_store.CreateDraft: create planner: %w", err)
	}
	hyperparameters, err := json.Marshal(nonNilObject(signup.Hyperparameters))
	if err != nil {
		return fmt.Errorf("platform_experiments_store.CreateDraft: encode hyperparameters: %w", err)
	}
	var addedBy domain.Actor
	if signup.Actor != nil {
		addedBy = *signup.Actor
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO experiment_signups (platform_experiment_id, agent_id, signed_up_at, role, quota_tier, hosted, launcher, flavor, hyperparameters, model, added_by_kind, added_by_id)
VALUES ($1, $2, NOW(), $3, $4, $5, $6, $7, $8, $9, $10, $11)`, pe.ID, signup.AgentID, string(signup.Role), string(signup.QuotaTier),
		signup.Hosted, signup.Launcher, signup.Flavor, hyperparameters, signup.Model, string(addedBy.Kind), addedBy.ID); err != nil {
		return fmt.Errorf("platform_experiments_store.CreateDraft: sign the planner up: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("platform_experiments_store.CreateDraft: commit: %w", err)
	}
	return nil
}

// ErrNotDraft is returned for a write that only a draft takes.
var ErrNotDraft = errors.New("platform experiment is not a draft")

// ErrPlanningOver is returned for a planner's answer to a draft whose planning limit has passed.
var ErrPlanningOver = errors.New("the draft's planning limit has passed")

// answerDraft runs a planner's answer, which a draft takes until its ends_at. The sweep closes a
// draft past it within a minute; an answer in that minute is refused here.
func (s *PlatformExperimentsStore) answerDraft(ctx context.Context, id, update string, args ...any) error {
	tag, err := s.pool.pool.Exec(ctx, update+` WHERE id = $1 AND status = 'draft' AND ends_at > now()`, append([]any{id}, args...)...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() > 0 {
		return nil
	}
	var draft bool
	if err := s.pool.pool.QueryRow(ctx, `SELECT COALESCE((SELECT status = 'draft' FROM platform_experiments WHERE id = $1), false)`, id).Scan(&draft); err != nil {
		return err
	}
	if draft {
		return ErrPlanningOver
	}
	return ErrNotDraft
}

// plannerKeptSQL is until when the planner of a draft that is answered now keeps its machine: $4
// seconds from now, so a revision asked for soon reaches the planner that is still there, and no
// later than the planning limit, which is the bound its machine was rented with. It reads ends_at
// as it was before the answer moved it, so an answer that replaces one leaves the moment alone.
const plannerKeptSQL = `CASE WHEN proposed_at IS NULL THEN LEAST(now() + make_interval(secs => $4), ends_at) ELSE planner_ends_at END`

// SetProposal stores what a draft's planner proposed, marks it proposed now, and moves the
// draft's end to waitUntil: from then on it waits for a person, and no longer for the planner,
// whose machine is kept for keepPlanner more (plannerKeptSQL).
func (s *PlatformExperimentsStore) SetProposal(ctx context.Context, id string, proposal []byte, waitUntil time.Time, keepPlanner time.Duration) error {
	err := s.answerDraft(ctx, id, `
UPDATE platform_experiments SET proposal = $2, proposed_at = now(), declined_reason = '', planner_ends_at = `+plannerKeptSQL+`, ends_at = $3, updated_at = now()`,
		proposal, waitUntil, keepPlanner.Seconds())
	if err != nil && !errors.Is(err, ErrNotDraft) && !errors.Is(err, ErrPlanningOver) {
		return fmt.Errorf("platform_experiments_store.SetProposal: %w", err)
	}
	return err
}

// Decline stores why a draft's planner proposes nothing. It answers the goal as a proposal
// does: the draft waits for a person until waitUntil, when the platform closes it as it closes
// a proposal nobody took up, and the planner's machine is kept for keepPlanner more, as after a
// proposal. A proposal made before no longer stands, so it is cleared and there is nothing to
// accept. proposed_at is the column of the planner's answer, whichever it was: with no proposal
// beside it the draft is declined.
func (s *PlatformExperimentsStore) Decline(ctx context.Context, id, reason string, waitUntil time.Time, keepPlanner time.Duration) error {
	err := s.answerDraft(ctx, id, `
UPDATE platform_experiments SET declined_reason = $2, proposal = NULL, proposed_at = now(), planner_ends_at = `+plannerKeptSQL+`, ends_at = $3, updated_at = now()`,
		reason, waitUntil, keepPlanner.Seconds())
	if err != nil && !errors.Is(err, ErrNotDraft) && !errors.Is(err, ErrPlanningOver) {
		return fmt.Errorf("platform_experiments_store.Decline: %w", err)
	}
	return err
}

// ErrNothingProposed is returned for accepting a draft whose planner has proposed nothing or
// declined, or whose proposal is being revised, and for revising one the planner has not
// answered.
var ErrNothingProposed = errors.New("nothing is proposed")

// AcceptDraft makes a proposed draft the open platform experiment pe describes and signs its
// researchers up, or does neither. pe carries what the proposal stated and what the person who
// accepts stated; its baseline is measured from its StartsAt, and it is open until its
// BaselineDueAt. Its planner has nothing left to answer, so the time its machine was kept for a
// revision ends with the accept. The limits a signup holds are held here too
// (ErrMaxAgentsReached, ErrMaxHostedReached).
func (s *PlatformExperimentsStore) AcceptDraft(ctx context.Context, pe *domain.PlatformExperiment, create []*domain.Agent, signups []domain.AgentSignup) error {
	if pe.Coordinator == nil || !pe.CoordinatorHosted {
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
	tx, err := s.pool.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("platform_experiments_store.AcceptDraft: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var status string
	var proposed bool
	err = tx.QueryRow(ctx, `SELECT status, proposed_at IS NOT NULL AND proposal IS NOT NULL FROM platform_experiments WHERE id = $1 FOR UPDATE`, pe.ID).Scan(&status, &proposed)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && status != string(domain.PlatformExpDraft)) {
		return ErrNotDraft
	}
	if err != nil {
		return fmt.Errorf("platform_experiments_store.AcceptDraft: lock draft: %w", err)
	}
	if !proposed {
		return ErrNothingProposed
	}
	if _, err := tx.Exec(ctx, `
UPDATE platform_experiments
SET status = 'open', name = $2, description = $3, budget_accelerator_hours = $4, max_agents = $5, metrics = $6,
    report_interval_seconds = $7, starts_at = $15, ends_at = $8, stages = $9, current_stage = 1,
    default_quota_tier = $10, max_concurrent_accelerators = $11,
    coordinator_hosted = true, coordinator_launcher = $12, coordinator_model = $13, duration_hours = $14, updated_at = now(),
    planner_ends_at = LEAST(planner_ends_at, now())
WHERE id = $1`, pe.ID, pe.Name, pe.Description, pe.BudgetAcceleratorHours, pe.MaxAgents, metrics,
		pe.ReportIntervalSeconds, pe.BaselineDueAt, stages,
		string(pe.DefaultQuotaTier), pe.MaxConcurrentAccelerators, pe.Coordinator.Launcher, pe.Coordinator.Model, pe.DurationHours, pe.StartsAt); err != nil {
		return fmt.Errorf("platform_experiments_store.AcceptDraft: %w", err)
	}
	if _, err := enrolTx(ctx, tx, pe.ID, create, signups, nil); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("platform_experiments_store.AcceptDraft: commit: %w", err)
	}
	return nil
}

// PlannerAnsweredAt returns the moment the run of the planner agentID ended: it answered its
// draft, with a proposal or with why there is none, and the time its machine is kept after an
// answer has passed (planner_ends_at). nil for an agent that is no planner, for a planner that
// is planning, which a revision makes true again, and for one kept after its answer.
func (s *PlatformExperimentsStore) PlannerAnsweredAt(ctx context.Context, platformExpID, agentID string) (*time.Time, error) {
	var answeredAt *time.Time
	err := s.pool.pool.QueryRow(ctx, `
SELECT CASE WHEN pe.planner_ends_at <= now() THEN pe.planner_ends_at END
FROM experiment_signups s JOIN platform_experiments pe ON pe.id = s.platform_experiment_id
WHERE s.platform_experiment_id = $1 AND s.agent_id = $2 AND s.role = '`+string(domain.SignupRolePlanner)+`'`, platformExpID, agentID).Scan(&answeredAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.PlannerAnsweredAt: %w", err)
	}
	return answeredAt, nil
}

// JobHasFinding says a finding was filed for the job.
func (s *PlatformExperimentsStore) JobHasFinding(ctx context.Context, experimentID string) (bool, error) {
	var found bool
	if err := s.pool.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM hypothesis_findings WHERE experiment_id = $1)`, experimentID).Scan(&found); err != nil {
		return false, fmt.Errorf("platform_experiments_store.JobHasFinding: %w", err)
	}
	return found, nil
}

// Revise records what a person asked a proposed draft's planner to change and gives the draft
// back to the planner: the proposal stays to be read, proposed_at and planner_ends_at are
// cleared, which is what the launcher keeps the planner's machine by, and the draft ends at
// planUntil unless the planner proposes again. The request is the planner's next update
// (platform_experiment.revision): one whose machine is still there answers it at once, and one
// whose machine was released is started again. ErrNotDraft, and ErrNothingProposed while the
// planner still works.
func (s *PlatformExperimentsStore) Revise(ctx context.Context, id, message string, by domain.Actor, planUntil time.Time) error {
	tx, err := s.pool.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("platform_experiments_store.Revise: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var status string
	var proposed bool
	err = tx.QueryRow(ctx, `SELECT status, proposed_at IS NOT NULL FROM platform_experiments WHERE id = $1 FOR UPDATE`, id).Scan(&status, &proposed)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && status != string(domain.PlatformExpDraft)) {
		return ErrNotDraft
	}
	if err != nil {
		return fmt.Errorf("platform_experiments_store.Revise: lock draft: %w", err)
	}
	if !proposed {
		return ErrNothingProposed
	}
	if _, err := tx.Exec(ctx, `UPDATE platform_experiments SET proposed_at = NULL, planner_ends_at = NULL, ends_at = $2, updated_at = now() WHERE id = $1`, id, planUntil); err != nil {
		return fmt.Errorf("platform_experiments_store.Revise: %w", err)
	}
	// A planner whose machine ended because its work was done is asked for under a new launch,
	// with its own restarts: a launch the launcher's reconcile does not know spends none of the
	// old one's. One whose machine is still there keeps it and its launch.
	if _, err := tx.Exec(ctx, `UPDATE experiment_signups SET instance_launch = '' WHERE platform_experiment_id = $1 AND role = $2 AND instance_name = ''`, id, string(domain.SignupRolePlanner)); err != nil {
		return fmt.Errorf("platform_experiments_store.Revise: renew the planner's launch: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO planning_requests (platform_experiment_id, message, actor_kind, actor_id, created_xid) VALUES ($1, $2, $3, $4, pg_current_xact_id()::text::bigint)`,
		id, message, string(by.Kind), by.ID); err != nil {
		return fmt.Errorf("platform_experiments_store.Revise: record request: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("platform_experiments_store.Revise: commit: %w", err)
	}
	return nil
}

func (s *PlatformExperimentsStore) planningRequests(ctx context.Context, id string) ([]domain.PlanningRequest, error) {
	rows, err := s.pool.pool.Query(ctx, `SELECT message, actor_kind, actor_id, created_at FROM planning_requests WHERE platform_experiment_id = $1 ORDER BY id`, id)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.planningRequests: %w", err)
	}
	defer rows.Close()
	var out []domain.PlanningRequest
	for rows.Next() {
		var request domain.PlanningRequest
		if err := rows.Scan(&request.Message, &request.Actor.Kind, &request.Actor.ID, &request.At); err != nil {
			return nil, fmt.Errorf("platform_experiments_store.planningRequests: %w", err)
		}
		out = append(out, request)
	}
	return out, rows.Err()
}
