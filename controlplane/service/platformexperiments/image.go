package platformexperiments

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/scaleresearch/hypothesisloop/contract/platformapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/imagedigest"
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/registrycred"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
)

// ImageResolver reads what an image reference names from its registry (imagedigest.Resolver).
type ImageResolver interface {
	Resolve(ctx context.Context, ref string, login *domain.RegistryLogin) (imagedigest.Image, error)
}

// WithImages wires the registry experiments push their images to, nil on a deployment that keeps
// none, and what reads an image from its registry.
func (s *Service) WithImages(registry registrycred.Provider, resolver ImageResolver) *Service {
	s.registry, s.images = registry, resolver
	return s
}

// ReasonNoRegistry refuses registry credentials on a deployment that keeps no registry.
const ReasonNoRegistry = "no_registry"

// ReasonImageOfAnotherOrg refuses an image in the repository of another org.
const ReasonImageOfAnotherOrg = "image_of_another_org"

// ReasonImageArchitecture refuses an image that does not say which architecture its jobs run on.
const ReasonImageArchitecture = "image_architecture"

// SetImage records the image ref names as what the jobs of platform experiment id submitted from
// now on run in, while it is open or running: the digest its registry answers for ref, of
// architecture, or of the one architecture the image runs on when architecture is empty. An image
// of the deployment's registry is read with a login of the org's own repository, and one of
// another org's repository is refused.
func (s *Service) SetImage(ctx context.Context, id, ref, architecture string) (*domain.ExperimentImage, error) {
	pe, err := s.stored(ctx, id)
	if err != nil {
		return nil, err
	}
	if pe.Status != domain.PlatformExpOpen && pe.Status != domain.PlatformExpRunning {
		return nil, refuseUpdate(http.StatusConflict, platformapi.ReasonExperimentClosed, "platform experiment %s is %s: its image is recorded while it is open or running", id, pe.Status)
	}
	orgID, err := s.clusters.ProjectOrg(ctx, pe.ProjectID)
	if err != nil {
		return nil, err
	}
	var login *domain.RegistryLogin
	if s.registry != nil && s.registry.Managed(ref) {
		if !s.registry.Holds(orgID, ref) {
			return nil, refuseUpdate(http.StatusUnprocessableEntity, ReasonImageOfAnotherOrg, "image %s is in the repository of another org: push the image to %s (hl image credentials)", ref, s.registry.Repository(orgID, id))
		}
		pull, err := s.registry.IssuePull(ctx, orgID)
		if err != nil {
			return nil, fmt.Errorf("platform_experiments.SetImage: pull login: %w", err)
		}
		login = &pull
	}
	image, err := s.images.Resolve(ctx, ref, login)
	if err != nil {
		return nil, refuseUpdate(http.StatusUnprocessableEntity, domain.ReasonImageUnresolved, "the registry of %s did not say which image it is: %s. Check that it is pushed and pullable, and record it again", ref, err)
	}
	chosen, err := domain.ImageArchitecture(image.Pinned, image.Architectures, architecture)
	if err != nil {
		return nil, refuseUpdate(http.StatusUnprocessableEntity, ReasonImageArchitecture, "%s", err)
	}
	recorded := domain.ExperimentImage{Image: image.Pinned, Architecture: chosen, SetAt: time.Now().UTC()}
	set, err := s.store.SetPlatformExperimentImage(ctx, id, recorded)
	if err != nil {
		return nil, err
	}
	if !set {
		return nil, refuseUpdate(http.StatusConflict, platformapi.ReasonExperimentClosed, "platform experiment %s closed: its image is recorded while it is open or running", id)
	}
	return &recorded, nil
}

// RegistryCredential is a short-lived login that pushes the images of platform experiment id to
// its org's repository, while it is open or running.
func (s *Service) RegistryCredential(ctx context.Context, id string) (*domain.RegistryCredential, error) {
	if s.registry == nil {
		return nil, refusal.Refuse(http.StatusConflict, ReasonNoRegistry, "this deployment keeps no registry: record an image any node pulls without a login (hl image set REF)")
	}
	pe, err := s.stored(ctx, id)
	if err != nil {
		return nil, err
	}
	if pe.Status != domain.PlatformExpOpen && pe.Status != domain.PlatformExpRunning {
		return nil, refuseUpdate(http.StatusConflict, platformapi.ReasonExperimentClosed, "platform experiment %s is %s: its images are pushed while it is open or running", id, pe.Status)
	}
	orgID, err := s.clusters.ProjectOrg(ctx, pe.ProjectID)
	if err != nil {
		return nil, err
	}
	login, err := s.registry.IssuePush(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.RegistryCredential: %w", err)
	}
	return &domain.RegistryCredential{RegistryLogin: login, Repository: s.registry.Repository(orgID, id)}, nil
}
