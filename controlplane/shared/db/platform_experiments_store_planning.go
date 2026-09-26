package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)


// CreateDraft creates a draft platform experiment with its planner, an agent of the draft's org
// signed up as the platform runs it, or creates neither. A draft takes no other signup: joining
// is for an open platform experiment (enrolTx), which a draft becomes when it is accepted.
func (s *PlatformExperimentsStore) CreateDraft(ctx context.Context, pe *domain.PlatformExperiment, planner *domain.Agent, signup domain.AgentSignup) error {
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
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("platform_experiments_store.CreateDraft: commit: %w", err)
	}
	return nil
}
