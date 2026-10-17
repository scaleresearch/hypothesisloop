package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// RepositoryToCreate is a platform experiment, a draft or not, whose repository the platform is
// to create, on Account.
type RepositoryToCreate struct {
	ID, ProjectID, Name, Account string
}

// awaitsRepository selects, of platform_experiments as pe, one whose repository is still to be
// created (domain.PlatformExperiment.AwaitsRepository). No machine of it is wanted until then.
const awaitsRepository = `pe.repository = '' AND pe.repository_account <> ''`

// ErrRepositoryNotAwaited is a repository recorded for a platform experiment that awaits none:
// it closed meanwhile, or was given one.
var ErrRepositoryNotAwaited = errors.New("platform experiment awaits no repository")

// RepositoriesToCreate returns the platform experiments not closed whose repository is still to
// be created, oldest first.
func (s *Store) RepositoriesToCreate(ctx context.Context) ([]RepositoryToCreate, error) {
	rows, err := s.pool.pool.Query(ctx, `
SELECT pe.id, pe.project_id, pe.name, pe.repository_account FROM platform_experiments pe
WHERE pe.status <> 'closed' AND `+awaitsRepository+`
ORDER BY pe.created_at`)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.RepositoriesToCreate: %w", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[RepositoryToCreate])
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.RepositoriesToCreate: %w", err)
	}
	return out, nil
}

// GiveRepository records the repository created for a platform experiment, from which its
// machines are started. ErrRepositoryNotAwaited when it closed meanwhile.
func (s *Store) GiveRepository(ctx context.Context, id, repository, url, branch string) error {
	tag, err := s.pool.pool.Exec(ctx, `
UPDATE platform_experiments pe SET repository = $2, repository_url = $3, branch = $4, updated_at = now()
WHERE pe.id = $1 AND pe.status <> 'closed' AND `+awaitsRepository, id, repository, url, branch)
	if err != nil {
		return fmt.Errorf("platform_experiments_store.GiveRepository: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrRepositoryNotAwaited
	}
	return nil
}

// FailRepository closes a platform experiment whose repository the git host did not create, with
// what it answered. One that closed meanwhile keeps what it has.
func (s *Store) FailRepository(ctx context.Context, id, answer string) error {
	return s.write(ctx, func(w *Write) error {
		if _, err := moveRun(ctx, w, `
UPDATE platform_experiments pe
SET status = 'closed', updated_at = now(), closed_at = now(), close_reason = '`+string(domain.CloseRepositoryNotCreated)+`', repository_error = $2, `+statusMoved+`
WHERE pe.id = $1 AND pe.status <> 'closed' AND `+awaitsRepository, id, answer); err != nil {
			return fmt.Errorf("platform_experiments_store.FailRepository: %w", err)
		}
		return nil
	})
}
