package registrycred

import (
	"context"
	"fmt"

	"github.com/scaleresearch/hypothesisloop/controlplane/clients/registrycred/gcp"
	"github.com/scaleresearch/hypothesisloop/controlplane/config"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// Provider is the registry the images of experiments are kept in, a repository for each org, and
// the logins to it. An agent pushes with a login of IssuePush, and a job's runtime pulls with one
// of IssuePull, each scoped as narrowly as the registry allows and short-lived. Neither ever
// carries an image through the control plane.
type Provider interface {
	// Repository is where the images of platform experiment peID of orgID are pushed.
	Repository(orgID, peID string) string
	// Holds says image is in the repository of orgID, and Managed that it is in any org's.
	Holds(orgID, image string) bool
	Managed(image string) bool
	// IssuePush is a login that writes to and reads from the repository of orgID.
	IssuePush(ctx context.Context, orgID string) (domain.RegistryLogin, error)
	// IssuePull is a login that only reads it.
	IssuePull(ctx context.Context, orgID string) (domain.RegistryLogin, error)
}

// New is the Provider the container_registry section names.
func New(ctx context.Context, r config.ContainerRegistryConfig) (Provider, error) {
	switch r.Provider {
	case config.RegistryProviderGCP:
		return gcp.New(ctx, r.GCP.Project, r.GCP.Location)
	}
	return nil, fmt.Errorf("registrycred: container_registry provider %q is not one this build knows", r.Provider)
}

// PullFor is the login a job of orgID pulls image with: one of provider for an image in the org's
// repository, and nil for any other, which is pulled without one. A nil provider is a deployment
// with no registry of its own.
func PullFor(ctx context.Context, provider Provider, orgID, image string) (*domain.RegistryLogin, error) {
	if provider == nil || !provider.Holds(orgID, image) {
		return nil, nil
	}
	login, err := provider.IssuePull(ctx, orgID)
	if err != nil {
		return nil, err
	}
	return &login, nil
}
