package registrycred

import (
	"context"
	"fmt"

	"github.com/scaleresearch/hypothesisloop/controlplane/clients/registrycred/gcp"
	"github.com/scaleresearch/hypothesisloop/controlplane/config"
)

// New is the registry the container_registry section names: the images of experiments are kept
// there, a repository for each org, and an agent pushes and a job's runtime pulls with
// short-lived logins scoped as narrowly as the registry allows. No image ever passes through the
// control plane.
func New(ctx context.Context, r config.ContainerRegistryConfig) (*gcp.Registry, error) {
	switch r.Provider {
	case config.RegistryProviderGCP:
		return gcp.New(ctx, r.GCP.Project, r.GCP.Location)
	}
	return nil, fmt.Errorf("registrycred: container_registry provider %q is not one this build knows", r.Provider)
}
