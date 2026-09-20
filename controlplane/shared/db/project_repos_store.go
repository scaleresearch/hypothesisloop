package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// ErrProjectRepoNotFound reports a project with no repository attached.
var ErrProjectRepoNotFound = errors.New("the project has no repository attached")

// AttachProjectRepo makes repo the project's repository, in place of any it had.
func (s *AccountsStore) AttachProjectRepo(ctx context.Context, repo *domain.ProjectRepo) error {
	err := s.pool.pool.QueryRow(ctx, `
INSERT INTO project_repos (project_id, installation_id, repository_id, repo)
VALUES ($1, $2, $3, $4)
ON CONFLICT (project_id) DO UPDATE SET
    installation_id = EXCLUDED.installation_id, repository_id = EXCLUDED.repository_id,
    repo = EXCLUDED.repo, attached_at = now()
RETURNING attached_at`, repo.ProjectID, repo.InstallationID, repo.RepositoryID, repo.Repo).Scan(&repo.AttachedAt)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgForeignKeyViolation {
		return ErrProjectNotFound
	}
	if err != nil {
		return fmt.Errorf("accounts_store.AttachProjectRepo: %w", err)
	}
	return nil
}

// GetProjectRepo returns the project's repository, or nil when none is attached.
func (s *AccountsStore) GetProjectRepo(ctx context.Context, projectID string) (*domain.ProjectRepo, error) {
	repo := &domain.ProjectRepo{}
	err := s.pool.pool.QueryRow(ctx, `
SELECT project_id, installation_id, repository_id, repo, attached_at
FROM project_repos WHERE project_id = $1`, projectID).
		Scan(&repo.ProjectID, &repo.InstallationID, &repo.RepositoryID, &repo.Repo, &repo.AttachedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("accounts_store.GetProjectRepo: %w", err)
	}
	return repo, nil
}

// DetachProjectRepo takes the project's repository away. ErrProjectRepoNotFound when it has none.
func (s *AccountsStore) DetachProjectRepo(ctx context.Context, projectID string) error {
	tag, err := s.pool.pool.Exec(ctx, `DELETE FROM project_repos WHERE project_id = $1`, projectID)
	if err != nil {
		return fmt.Errorf("accounts_store.DetachProjectRepo: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrProjectRepoNotFound
	}
	return nil
}
