package environments

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/githost"
)

// Store keeps the environments (db.EnvironmentsStore).
type Store interface {
	EnsureEnvironment(ctx context.Context, env *domain.Environment) (*domain.Environment, error)
	GetEnvironment(ctx context.Context, key string) (*domain.Environment, error)
	ListBuildingEnvironments(ctx context.Context) ([]*domain.Environment, error)
	FinishEnvironment(ctx context.Context, key string, state domain.EnvironmentState, logTail string) error
}

// Service finds the environment a repository declares and makes it known to the builder.
type Service struct {
	store Store
	host  githost.Provider
	// repository is where every environment image is pushed, tagged with its key.
	repository string
}

func NewService(store Store, host githost.Provider, repository string) *Service {
	if store == nil || host == nil || repository == "" {
		panic("environments.NewService: the store, the git host and the image repository are required")
	}
	return &Service{store: store, host: host, repository: repository}
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

// Ensure returns the environment of dockerfile built on base, to be built when it is new. A file
// the platform does not build is a *domain.EnvironmentFileError.
func (s *Service) Ensure(ctx context.Context, dockerfile, base string) (*domain.Environment, error) {
	if err := domain.ValidateEnvironmentFile(dockerfile); err != nil {
		return nil, err
	}
	key := domain.EnvironmentKey(asRoot(dockerfile), base)
	env, err := s.store.EnsureEnvironment(ctx, &domain.Environment{Key: key, Base: base, Dockerfile: dockerfile, Image: s.repository + ":" + key})
	if err != nil {
		return nil, fmt.Errorf("environments: %w", err)
	}
	return env, nil
}

// Declared returns the environment the repository's commit ref declares, built on base, nil
// when it declares none.
func (s *Service) Declared(ctx context.Context, projectID, repository, ref, base string) (*domain.Environment, error) {
	file, err := s.Read(ctx, projectID, repository, ref)
	if err != nil || file == nil {
		return nil, err
	}
	return s.Ensure(ctx, *file, base)
}

// baseUserArg is the build argument that carries the user of the base image.
const baseUserArg = "HL_BASE_USER"

// asRoot is what is built of a valid environment file: its instructions run as root and the user
// of the base image, passed as baseUserArg, is restored after them. The job images run as root and
// the agent image as agent, and one file installs system packages on both.
func asRoot(dockerfile string) string {
	lines := strings.SplitAfter(dockerfile, "\n")
	instructions := 0
	for i, line := range lines {
		fields := strings.Fields(line)
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		if instructions++; instructions < 2 {
			continue
		}
		from := strings.TrimSuffix(strings.Join(lines[:i+1], ""), "\n")
		rest := strings.TrimSuffix(strings.Join(lines[i+1:], ""), "\n")
		return from + "\nUSER root\n" + rest + "\nARG " + baseUserArg + "\nUSER ${" + baseUserArg + "}\n"
	}
	panic("environments.asRoot: a file domain.ValidateEnvironmentFile refuses")
}

// Get returns the environment of key, nil when there is none.
func (s *Service) Get(ctx context.Context, key string) (*domain.Environment, error) {
	return s.store.GetEnvironment(ctx, key)
}
