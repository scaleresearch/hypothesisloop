package registrycred

import (
	"context"
	"fmt"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/clients/registrycred/gcp"
	"github.com/scaleresearch/hypothesisloop/controlplane/config"
)

// New is the registry the container_registry section names: the images of experiments are kept
// there, a repository for each org, and an agent pushes and a job's runtime pulls with
// short-lived logins scoped as narrowly as the registry allows. No image ever passes through the
// control plane. keepFor is how long after its push a version of an image may still be pulled by
// a job, so the registry deletes one only once it is older and not the newest of its image.
func New(ctx context.Context, r config.ContainerRegistryConfig, keepFor time.Duration) (*gcp.Registry, error) {
	switch r.Provider {
	case config.RegistryProviderGCP:
		return gcp.New(ctx, r.GCP.Project, r.GCP.Location, keepFor)
	}
	return nil, fmt.Errorf("registrycred: container_registry provider %q is not one this build knows", r.Provider)
}
