// Package registrycred hands out short-lived logins to the registry of environment images. Each
// registry provider mints them its own way, in a sub-package of its own.
package registrycred

import (
	"context"
	"fmt"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/config"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/registrycred/gcp"
)

// Source hands out the logins of one registry repository.
type Source interface {
	// Push is a login that writes to the repository.
	Push(ctx context.Context) (domain.RegistryLogin, error)
	// Pull is a login that reads from it.
	Pull(ctx context.Context) (domain.RegistryLogin, error)
}

// New is the Source of the provider the environments section names.
func New(ctx context.Context, e config.EnvironmentsConfig) (Source, error) {
	switch e.Provider {
	case config.RegistryProviderGCP:
		return gcp.New(ctx, e.Repository, e.GCP.Pusher, e.GCP.Puller)
	}
	return nil, fmt.Errorf("registrycred: environments provider %q is not one this build knows", e.Provider)
}
