package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// ErrExperimentDraftNotFound reports a draft the project does not hold.
var ErrExperimentDraftNotFound = errors.New("experiment draft not found")

const experimentDraftColumns = `id, project_id, form, step, failure, created_at, updated_at`

func scanExperimentDraft(row rowScanner) (*domain.ExperimentDraft, error) {
	d := &domain.ExperimentDraft{}
	if err := row.Scan(&d.ID, &d.ProjectID, &d.Form, &d.Step, &d.Failure, &d.CreatedAt, &d.UpdatedAt); err != nil {
		return nil, err
	}
	return d, nil
}

// CreateExperimentDraft stores a new draft in its project under an id it issues, and returns it
// as stored: ErrProjectNotFound for a project that is not there.
func (s *Store) CreateExperimentDraft(ctx context.Context, projectID string, form []byte, step, failure string) (*domain.ExperimentDraft, error) {
	id, err := domain.NewID(domain.ExperimentDraftIDPrefix)
	if err != nil {
		return nil, err
	}
	d, err := scanExperimentDraft(s.pool.pool.QueryRow(ctx, `
INSERT INTO experiment_drafts (id, project_id, form, step, failure) VALUES ($1, $2, $3, $4, $5)
RETURNING `+experimentDraftColumns, id, projectID, form, step, failure))
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgForeignKeyViolation {
		return nil, ErrProjectNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("experiment_drafts_store.CreateExperimentDraft: %w", err)
	}
	return d, nil
}

// ListExperimentDrafts returns the drafts of a project, the one changed last first.
func (s *Store) ListExperimentDrafts(ctx context.Context, projectID string) ([]*domain.ExperimentDraft, error) {
	rows, err := s.pool.pool.Query(ctx, `
SELECT `+experimentDraftColumns+` FROM experiment_drafts WHERE project_id = $1 ORDER BY updated_at DESC, id`, projectID)
	if err != nil {
		return nil, fmt.Errorf("experiment_drafts_store.ListExperimentDrafts: %w", err)
	}
	defer rows.Close()
	out := []*domain.ExperimentDraft{}
	for rows.Next() {
		d, err := scanExperimentDraft(rows)
		if err != nil {
			return nil, fmt.Errorf("experiment_drafts_store.ListExperimentDrafts: scan: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// GetExperimentDraft returns a draft of the project: ErrExperimentDraftNotFound for one it does
// not hold, whether or not another project does.
func (s *Store) GetExperimentDraft(ctx context.Context, projectID, id string) (*domain.ExperimentDraft, error) {
	d, err := scanExperimentDraft(s.pool.pool.QueryRow(ctx, `
SELECT `+experimentDraftColumns+` FROM experiment_drafts WHERE project_id = $1 AND id = $2`, projectID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrExperimentDraftNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("experiment_drafts_store.GetExperimentDraft: %w", err)
	}
	return d, nil
}

// UpdateExperimentDraft replaces what a draft of the project holds and returns it as stored.
func (s *Store) UpdateExperimentDraft(ctx context.Context, projectID, id string, form []byte, step, failure string) (*domain.ExperimentDraft, error) {
	d, err := scanExperimentDraft(s.pool.pool.QueryRow(ctx, `
UPDATE experiment_drafts SET form = $3, step = $4, failure = $5, updated_at = now()
WHERE project_id = $1 AND id = $2
RETURNING `+experimentDraftColumns, projectID, id, form, step, failure))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrExperimentDraftNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("experiment_drafts_store.UpdateExperimentDraft: %w", err)
	}
	return d, nil
}

// DeleteExperimentDraft removes a draft of the project.
func (s *Store) DeleteExperimentDraft(ctx context.Context, projectID, id string) error {
	tag, err := s.pool.pool.Exec(ctx, `DELETE FROM experiment_drafts WHERE project_id = $1 AND id = $2`, projectID, id)
	if err != nil {
		return fmt.Errorf("experiment_drafts_store.DeleteExperimentDraft: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrExperimentDraftNotFound
	}
	return nil
}
