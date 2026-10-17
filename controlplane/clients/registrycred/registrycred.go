package registrycred

import (
	"context"
	"fmt"

	"github.com/scaleresearch/hypothesisloop/controlplane/clients/registrycred/gcp"
	"github.com/scaleresearch/hypothesisloop/controlplane/config"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// Source hands out the logins of one registry repository.
type Source interface {
	// Push is a login that writes to the repository.
	Push(ctx context.Context) (domain.RegistryLogin, error)
	// Pull is a login that reads from it.
	Pull(ctx context.Context) (domain.RegistryLogin, error)
}

// New is the Source of the provider the container_registry section names, for its environments
// repository.
func New(ctx context.Context, r config.ContainerRegistryConfig) (Source, error) {
	switch r.Provider {
	case config.RegistryProviderGCP:
		return gcp.New(ctx, r.EnvironmentsRepository, r.GCP.Pusher, r.GCP.Puller)
	}
	return nil, fmt.Errorf("registrycred: container_registry provider %q is not one this build knows", r.Provider)
}
