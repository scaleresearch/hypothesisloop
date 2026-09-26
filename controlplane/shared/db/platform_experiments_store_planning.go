package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)


// CreateDraft creates a draft platform experiment with its planner, an agent of the draft's org
// signed up as the platform runs it, or creates neither. A draft takes no other signup: joining
// is for an open platform experiment (enrolTx), which a draft becomes when it is accepted.
//
// limits bound the org's drafts, counted under a lock on the org so that two drafts created at
// once cannot both be the last one allowed: *PlanningLimitError when one is reached.
func (s *PlatformExperimentsStore) CreateDraft(ctx context.Context, pe *domain.PlatformExperiment, planner *domain.Agent, signup domain.AgentSignup, limits DraftLimits) error {
	if pe.Status != domain.PlatformExpDraft || signup.AgentID != planner.ID || signup.Flavor != domain.PlannerFlavor || !signup.Hosted {
		return fmt.Errorf("platform_experiments_store.CreateDraft: a draft is created with status draft and its hosted planner, got status %s and signup %+v", pe.Status, signup)
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
	if _, err := tx.Exec(ctx, `SELECT 1 FROM orgs WHERE id = $1 FOR UPDATE`, planner.OrgID); err != nil {
		return fmt.Errorf("platform_experiments_store.CreateDraft: lock org: %w", err)
	}
	var open, today int
	if err := tx.QueryRow(ctx, `
SELECT COUNT(*) FILTER (WHERE pe.status = 'draft'), COUNT(*) FILTER (WHERE pe.created_at > now() - interval '24 hours')
FROM platform_experiments pe JOIN projects p ON p.id = pe.project_id
WHERE p.org_id = $1 AND pe.goal <> ''`, planner.OrgID).Scan(&open, &today); err != nil {
		return fmt.Errorf("platform_experiments_store.CreateDraft: count drafts: %w", err)
	}
	if open >= limits.Concurrent {
		return &PlanningLimitError{Limit: limits.Concurrent}
	}
	if today >= limits.PerDay {
		return &PlanningLimitError{Limit: limits.PerDay, PerDay: true}
	}
	if err := insertPlatformExperiment(ctx, tx, pe); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO agents (id, name, org_id, kind, performance_score, created_at)
VALUES ($1, $2, $3, $4, $5, $6)`, planner.ID, planner.Name, planner.OrgID, planner.Kind, planner.PerformanceScore, planner.CreatedAt); err != nil {
		return fmt.Errorf("platform_experiments_store.CreateDraft: create planner: %w", err)
	}
	hyperparameters, err := json.Marshal(nonNilObject(signup.Hyperparameters))
	if err != nil {
		return fmt.Errorf("platform_experiments_store.CreateDraft: encode hyperparameters: %w", err)
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO experiment_signups (platform_experiment_id, agent_id, signed_up_at, role, quota_tier, hosted, launcher, flavor, hyperparameters, model)
VALUES ($1, $2, NOW(), $3, $4, $5, $6, $7, $8, $9)`, pe.ID, signup.AgentID, string(signup.Role), string(signup.QuotaTier),
		signup.Hosted, signup.Launcher, signup.Flavor, hyperparameters, signup.Model); err != nil {
		return fmt.Errorf("platform_experiments_store.CreateDraft: sign the planner up: %w", err)
	}
	// The planner's quota is the draft's whole budget, the planning allowance: its baseline job
	// is admitted against it like any job against its agent's quota.
	if _, err := tx.Exec(ctx, upsertAgentQuotaSQL, agentQuotaUpsertArgs(&domain.AgentQuota{
		ID: uuid.New().String(), AgentID: planner.ID, PlatformExperimentID: pe.ID,
		GuaranteedAcceleratorHours: pe.BudgetAcceleratorHours, CreatedAt: pe.CreatedAt,
	})...); err != nil {
		return fmt.Errorf("platform_experiments_store.CreateDraft: allocate the planner's quota: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("platform_experiments_store.CreateDraft: commit: %w", err)
	}
	return nil
}

// DraftLimits bound the drafts of one org: how many may be open at once and how many it may
// create in 24 hours.
type DraftLimits struct {
	Concurrent int
	PerDay     int
}

// PlanningLimitError is a draft refused because its org reached Limit drafts, open at once or,
// with PerDay, created in the last 24 hours.
type PlanningLimitError struct {
	Limit  int
	PerDay bool
}

func (e *PlanningLimitError) Error() string {
	if e.PerDay {
		return fmt.Sprintf("the org created %d drafts in the last 24 hours", e.Limit)
	}
	return fmt.Sprintf("the org has %d drafts open", e.Limit)
}

// ErrNotDraft is returned for a write that only a draft takes.
var ErrNotDraft = errors.New("platform experiment is not a draft")

// SetProposal stores what a draft's planner proposed, marks it proposed now, and moves the
// draft's end to waitUntil: from then on it waits for a person, and no longer for the planner.
func (s *PlatformExperimentsStore) SetProposal(ctx context.Context, id string, proposal []byte, waitUntil time.Time) error {
	tag, err := s.pool.pool.Exec(ctx, `
UPDATE platform_experiments SET proposal = $2, proposed_at = now(), ends_at = $3, updated_at = now()
WHERE id = $1 AND status = 'draft'`, id, proposal, waitUntil)
	if err != nil {
		return fmt.Errorf("platform_experiments_store.SetProposal: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotDraft
	}
	return nil
}

// ErrNothingProposed is returned for accepting a draft whose planner has proposed nothing, or
// whose proposal is being revised.
var ErrNothingProposed = errors.New("nothing is proposed")

// AcceptDraft makes a proposed draft the open platform experiment pe describes and signs its
// researchers up, or does neither. pe carries what the proposal stated and what the person who
// accepts stated; its start is now, on the database's clock, so the auto-start sweep starts it
// if nobody does. The limits a signup holds are held here too (ErrMaxAgentsReached,
// ErrMaxHostedReached).
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
	err = tx.QueryRow(ctx, `SELECT status, proposed_at IS NOT NULL FROM platform_experiments WHERE id = $1 FOR UPDATE`, pe.ID).Scan(&status, &proposed)
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
    report_interval_seconds = $7, starts_at = now(), ends_at = $8, stages = $9, current_stage = 1,
    hypothesis_submit_policy = $10, job_submit_policy = $11, default_quota_tier = $12, max_concurrent_accelerators = $13,
    coordinator_hosted = true, coordinator_launcher = $14, coordinator_model = $15, updated_at = now()
WHERE id = $1`, pe.ID, pe.Name, pe.Description, pe.BudgetAcceleratorHours, pe.MaxAgents, metrics,
		pe.ReportIntervalSeconds, pe.EndsAt, stages, string(pe.HypothesisSubmitPolicy), string(pe.JobSubmitPolicy),
		string(pe.DefaultQuotaTier), pe.MaxConcurrentAccelerators, pe.Coordinator.Launcher, pe.Coordinator.Model); err != nil {
		return fmt.Errorf("platform_experiments_store.AcceptDraft: %w", err)
	}
	if _, err := enrolTx(ctx, tx, pe.ID, create, signups); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("platform_experiments_store.AcceptDraft: commit: %w", err)
	}
	return nil
}

// JobHasFinding says a finding was filed for the job.
func (s *PlatformExperimentsStore) JobHasFinding(ctx context.Context, experimentID string) (bool, error) {
	var found bool
	if err := s.pool.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM hypothesis_findings WHERE experiment_id = $1)`, experimentID).Scan(&found); err != nil {
		return false, fmt.Errorf("platform_experiments_store.JobHasFinding: %w", err)
	}
	return found, nil
}
