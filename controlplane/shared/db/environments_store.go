package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// EnvironmentsStore keeps the environments the platform builds (domain.Environment).
type EnvironmentsStore struct {
	pool *Pool
}

func NewEnvironmentsStore(pool *Pool) *EnvironmentsStore {
	return &EnvironmentsStore{pool: pool}
}

const environmentColumns = ` key, base, dockerfile, image, state, log_tail, created_at, updated_at `

func scanEnvironment(row rowScanner) (*domain.Environment, error) {
	env := &domain.Environment{}
	var state string
	if err := row.Scan(&env.Key, &env.Base, &env.Dockerfile, &env.Image, &state, &env.LogTail, &env.CreatedAt, &env.UpdatedAt); err != nil {
		return nil, err
	}
	env.State = domain.EnvironmentState(state)
	return env, nil
}

// EnsureEnvironment returns the environment of env's key as it is stored, writing env as building
// when there is none.
func (s *EnvironmentsStore) EnsureEnvironment(ctx context.Context, env *domain.Environment) (*domain.Environment, error) {
	const q = `WITH made AS (
	INSERT INTO environments (key, base, dockerfile, image, state) VALUES ($1, $2, $3, $4, 'building')
	ON CONFLICT (key) DO NOTHING
	RETURNING` + environmentColumns + `)
SELECT` + environmentColumns + `FROM made
UNION ALL
SELECT` + environmentColumns + `FROM environments WHERE key = $1
LIMIT 1`
	stored, err := scanEnvironment(s.pool.pool.QueryRow(ctx, q, env.Key, env.Base, env.Dockerfile, env.Image))
	if err != nil {
		return nil, fmt.Errorf("environments_store.Ensure: %w", err)
	}
	return stored, nil
}

// GetEnvironment returns the environment of key, nil when there is none.
func (s *EnvironmentsStore) GetEnvironment(ctx context.Context, key string) (*domain.Environment, error) {
	env, err := scanEnvironment(s.pool.pool.QueryRow(ctx, `SELECT`+environmentColumns+`FROM environments WHERE key = $1`, key))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("environments_store.Get: %w", err)
	}
	return env, nil
}

// ListBuildingEnvironments returns the environments still to be built, oldest first.
func (s *EnvironmentsStore) ListBuildingEnvironments(ctx context.Context) ([]*domain.Environment, error) {
	rows, err := s.pool.pool.Query(ctx, `SELECT`+environmentColumns+`FROM environments WHERE state = 'building' ORDER BY created_at, key`)
	if err != nil {
		return nil, fmt.Errorf("environments_store.ListBuilding: %w", err)
	}
	defer rows.Close()
	var out []*domain.Environment
	for rows.Next() {
		env, err := scanEnvironment(rows)
		if err != nil {
			return nil, fmt.Errorf("environments_store.ListBuilding: %w", err)
		}
		out = append(out, env)
	}
	return out, rows.Err()
}

// FinishEnvironment records the outcome of a build, ready or failed, once.
func (s *EnvironmentsStore) FinishEnvironment(ctx context.Context, key string, state domain.EnvironmentState, logTail string) error {
	if state != domain.EnvironmentReady && state != domain.EnvironmentFailed {
		return fmt.Errorf("environments_store.Finish: %q is no outcome of a build", state)
	}
	_, err := s.pool.pool.Exec(ctx, `UPDATE environments SET state = $2, log_tail = $3, updated_at = now() WHERE key = $1 AND state = 'building'`, key, string(state), logTail)
	if err != nil {
		return fmt.Errorf("environments_store.Finish: %w", err)
	}
	return nil
}
