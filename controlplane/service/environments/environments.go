package environments

import (
	"context"
	"errors"
	"fmt"

	"github.com/scaleresearch/hypothesisloop/controlplane/clients/githost"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// Store keeps the environments (db.Store).
type Store interface {
	OrgHasEnvironmentBuilder(ctx context.Context, orgID string) (bool, error)
	EnsureEnvironment(ctx context.Context, env *domain.Environment) (*domain.Environment, error)
	GetEnvironment(ctx context.Context, key string) (*domain.Environment, error)
}

// Images names an image reference by the digest of what it names now, repository@digest
// (imagedigest.Resolver).
type Images interface {
	Pinned(ctx context.Context, ref string) (string, error)
}

// Service finds the environment a repository declares and records it to be built.
type Service struct {
	store   Store
	host    githost.Provider
	offered []domain.JobImage
	images  Images
}

func NewService(store Store, host githost.Provider) *Service {
	if store == nil || host == nil {
		panic("environments.NewService: the store and the git host are required")
	}
	return &Service{store: store, host: host}
}

// WithJobImages has an environment of a job naming an offered image that has an environment base
// built on that base with the image's tools last (domain.JobImage.EnvironmentBase), and every
// image it is built from named by its digest, by images, so it is keyed on what the images hold
// and not on their tags.
func (s *Service) WithJobImages(offered []domain.JobImage, images Images) *Service {
	s.offered, s.images = offered, images
	return s
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

// ensure returns the environment of dockerfile of the project's repository for a job of orgID
// naming image, to be built when it is new or failed. A file the platform does not build is a
// *domain.EnvironmentFileError, an org no cluster builds for domain.ErrNoEnvironmentBuilder, and
// an image whose registry does not name its digest a *domain.ImageUnresolvedError.
func (s *Service) ensure(ctx context.Context, orgID, projectID, repository, dockerfile, image string) (*domain.Environment, error) {
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
	base, tools, err := s.builtFrom(ctx, image)
	if err != nil {
		return nil, err
	}
	env, err := s.store.EnsureEnvironment(ctx, &domain.Environment{Key: domain.EnvironmentKey(orgID, repository, dockerfile, base, tools), OrgID: orgID,
		ProjectID: projectID, GitRepository: repository, Base: base, Tools: tools, Dockerfile: dockerfile})
	if err != nil {
		return nil, fmt.Errorf("environments: %w", err)
	}
	return env, nil
}

// builtFrom is the base an environment of a job naming image is built on, and the image its
// tools come from, none when it is built on image itself, each named by its digest.
func (s *Service) builtFrom(ctx context.Context, image string) (base, tools string, err error) {
	base = image
	for _, offered := range s.offered {
		if offered.Image == image && offered.EnvironmentBase != "" {
			base, tools = offered.EnvironmentBase, image
		}
	}
	if s.images == nil {
		return base, tools, nil
	}
	if base, err = s.pinned(ctx, base); err != nil {
		return "", "", err
	}
	if tools != "" {
		if tools, err = s.pinned(ctx, tools); err != nil {
			return "", "", err
		}
	}
	return base, tools, nil
}

func (s *Service) pinned(ctx context.Context, ref string) (string, error) {
	pinned, err := s.images.Pinned(ctx, ref)
	if err != nil {
		return "", &domain.ImageUnresolvedError{Image: ref, Err: err}
	}
	return pinned, nil
}

// Declared returns the environment the repository's commit ref declares for a job of orgID
// naming image, nil when it declares none.
func (s *Service) Declared(ctx context.Context, orgID, projectID, repository, ref, image string) (*domain.Environment, error) {
	file, err := s.Read(ctx, projectID, repository, ref)
	if err != nil || file == nil {
		return nil, err
	}
	return s.ensure(ctx, orgID, projectID, repository, *file, image)
}

// Find returns the environment the repository's commit ref declares for a job of orgID naming
// image as it is, and builds nothing: nil when the commit declares none, and declared false with
// it, or when it was never asked for.
func (s *Service) Find(ctx context.Context, orgID, projectID, repository, ref, image string) (env *domain.Environment, declared bool, err error) {
	file, err := s.Read(ctx, projectID, repository, ref)
	if err != nil || file == nil {
		return nil, false, err
	}
	if err := domain.ValidateEnvironmentFile(*file); err != nil {
		return nil, true, err
	}
	base, tools, err := s.builtFrom(ctx, image)
	if err != nil {
		return nil, true, err
	}
	env, err = s.store.GetEnvironment(ctx, domain.EnvironmentKey(orgID, repository, *file, base, tools))
	if err != nil {
		return nil, true, fmt.Errorf("environments: %w", err)
	}
	return env, true, nil
}

// Get returns the environment of key, nil when there is none.
func (s *Service) Get(ctx context.Context, key string) (*domain.Environment, error) {
	return s.store.GetEnvironment(ctx, key)
}
