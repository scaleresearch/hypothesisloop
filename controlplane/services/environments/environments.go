package environments

import (
	"context"
	"errors"
	"fmt"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/githost"
)

// Store keeps the environments (db.EnvironmentsStore).
type Store interface {
	OrgHasEnvironmentBuilder(ctx context.Context, orgID string) (bool, error)
	EnsureEnvironment(ctx context.Context, env *domain.Environment) (*domain.Environment, error)
	GetEnvironment(ctx context.Context, key string) (*domain.Environment, error)
}

// Service finds the environment a repository declares and records it to be built.
type Service struct {
	store Store
	host  githost.Provider
}

func NewService(store Store, host githost.Provider) *Service {
	if store == nil || host == nil {
		panic("environments.NewService: the store and the git host are required")
	}
	return &Service{store: store, host: host}
}

// Read returns the environment file of the repository at ref, nil when it has none there.
func (s *Service) Read(ctx context.Context, projectID, repository, ref string) (*string, error) {
	data, err := s.host.FileAt(ctx, projectID, repository, ref, domain.EnvironmentFile)
	if errors.Is(err, githost.ErrFileNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	file := string(data)
	return &file, nil
}

// ensure returns the environment of dockerfile built on base for orgID, to be built when it is
// new or failed. A file the platform does not build is a *domain.EnvironmentFileError, and an org
// no cluster builds for domain.ErrNoEnvironmentBuilder.
func (s *Service) ensure(ctx context.Context, orgID, dockerfile, base string) (*domain.Environment, error) {
	if err := domain.ValidateEnvironmentFile(dockerfile); err != nil {
		return nil, err
	}
	builds, err := s.store.OrgHasEnvironmentBuilder(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("environments: %w", err)
	}
	if !builds {
		return nil, domain.ErrNoEnvironmentBuilder
	}
	env, err := s.store.EnsureEnvironment(ctx, &domain.Environment{Key: domain.EnvironmentKey(orgID, dockerfile, base), OrgID: orgID, Base: base, Dockerfile: dockerfile})
	if err != nil {
		return nil, fmt.Errorf("environments: %w", err)
	}
	return env, nil
}

// Declared returns the environment the repository's commit ref declares, built on base for
// orgID, nil when it declares none.
func (s *Service) Declared(ctx context.Context, orgID, projectID, repository, ref, base string) (*domain.Environment, error) {
	file, err := s.Read(ctx, projectID, repository, ref)
	if err != nil || file == nil {
		return nil, err
	}
	return s.ensure(ctx, orgID, *file, base)
}

// Get returns the environment of key, nil when there is none.
func (s *Service) Get(ctx context.Context, key string) (*domain.Environment, error) {
	return s.store.GetEnvironment(ctx, key)
}
