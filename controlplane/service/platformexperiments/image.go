package platformexperiments

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/contract/platformapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
)

// ImageResolver reads what an image reference names from its registry (imagedigest.Resolver).
type ImageResolver interface {
	Resolve(ctx context.Context, ref string, login *domain.RegistryLogin) (domain.ResolvedImage, error)
}

// ImageRegistry is the registry experiments push their images to, a repository for each org
// (registrycred).
type ImageRegistry interface {
	// Repository is where the images of platform experiment peID of orgID are pushed.
	Repository(orgID, peID string) string
	// Holds says image is in the repository of orgID, and Managed that it is in any org's.
	Holds(orgID, image string) bool
	Managed(image string) bool
	// Provision makes the repository of orgID and what its logins need, once and for good. It
	// may take minutes, and is done by the registry pass, never in a request.
	Provision(ctx context.Context, orgID string) error
	// PushLogin is a login that writes to and reads from the repository of orgID, once it is
	// provisioned.
	PushLogin(ctx context.Context, orgID string) (domain.RegistryLogin, error)
	// PullLogin is the login a job of orgID pulls image with, nil for an image pulled without one.
	PullLogin(ctx context.Context, orgID, image string) (*domain.RegistryLogin, error)
}

// OrgRegistries records which orgs' repositories are wanted and which are provisioned (db.Store).
type OrgRegistries interface {
	// AskOrgRegistry records that orgID's repository is wanted, and says whether it is
	// provisioned.
	AskOrgRegistry(ctx context.Context, orgID string) (provisioned bool, err error)
	// OrgRegistriesToProvision are the orgs whose repository is wanted and not provisioned, and
	// with every the provisioned ones too.
	OrgRegistriesToProvision(ctx context.Context, every bool) ([]string, error)
	// RecordOrgRegistry records the provisioning of orgID's repository: done, or failed with
	// failure.
	RecordOrgRegistry(ctx context.Context, orgID, failure string) error
}

// WithImages wires the registry experiments push their images to, nil on a deployment that keeps
// none, where its orgs' repositories are recorded, and what reads an image from its registry.
func (s *Service) WithImages(registry ImageRegistry, orgRegistries OrgRegistries, resolver ImageResolver) *Service {
	s.registry, s.orgRegistries, s.images = registry, orgRegistries, resolver
	return s
}

// ReasonNoRegistry refuses registry credentials on a deployment that keeps no registry.
const ReasonNoRegistry = "no_registry"

// ReasonImageOfAnotherOrg refuses an image in the repository of another org.
const ReasonImageOfAnotherOrg = "image_of_another_org"

// ReasonImageArchitecture refuses an image that does not say which architecture its jobs run on.
const ReasonImageArchitecture = "image_architecture"

// ReasonRegistryUnavailable answers a login the registry's provider failed to issue, which a
// later ask may get.
const ReasonRegistryUnavailable = "registry_unavailable"

// registryUnavailable is the refusal of a login the registry's provider failed to issue: what
// failed is logged, and the caller is told to ask again.
func (s *Service) registryUnavailable(what string, err error) error {
	s.logger.Warn("the registry's provider issued no login", zap.String("login", what), zap.Error(err))
	return refusal.Refuse(http.StatusServiceUnavailable, ReasonRegistryUnavailable, "the provider of the platform's container registry failed to issue a "+what+" login just now: ask again in a minute")
}

// ReasonRegistryPreparing answers a push login of an org whose repository the registry pass has
// not provisioned yet.
const ReasonRegistryPreparing = "registry_preparing"

// SetImage records the image ref names as what the jobs of platform experiment id submitted from
// now on run in, while it is open or running: the digest its registry answers for ref, of
// architecture, or of the one architecture the image runs on when architecture is empty. An image
// of the deployment's registry is read with a login of the org's own repository, and one of
// another org's repository is refused.
func (s *Service) SetImage(ctx context.Context, id, ref, architecture string) (*platformapi.ExperimentImage, error) {
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
	if s.registry != nil {
		if s.registry.Managed(ref) && !s.registry.Holds(orgID, ref) {
			return nil, refuseUpdate(http.StatusUnprocessableEntity, ReasonImageOfAnotherOrg, "image %s is in the repository of another org: push the image to %s (hl image repository)", ref, s.registry.Repository(orgID, id))
		}
		if login, err = s.registry.PullLogin(ctx, orgID, ref); err != nil {
			return nil, s.registryUnavailable("pull", err)
		}
	}
	image, err := s.images.Resolve(ctx, ref, login)
	if err != nil {
		return nil, refuseUpdate(http.StatusUnprocessableEntity, domain.ReasonImageUnresolved, "the registry of %s did not say which image it is: %s. Check that it is pushed and pullable, and record it again", ref, err)
	}
	chosen, err := domain.ImageArchitecture(image.Pinned, image.Architectures, architecture)
	if err != nil {
		return nil, refuseUpdate(http.StatusUnprocessableEntity, ReasonImageArchitecture, "%s", err)
	}
	recorded := platformapi.ExperimentImage{Image: image.Pinned, Architecture: chosen, SetAt: time.Now().UTC()}
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
// its org's repository, while it is open or running. The first asks for the org's repository to
// be provisioned, which the registry pass does off the request, and is answered 503
// registry_preparing until it is.
func (s *Service) RegistryCredential(ctx context.Context, id string) (*platformapi.RegistryCredential, error) {
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
	provisioned, err := s.orgRegistries.AskOrgRegistry(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.RegistryCredential: %w", err)
	}
	if !provisioned {
		select {
		case s.registryWake <- struct{}{}:
		default:
		}
		return nil, refusal.Refuse(http.StatusServiceUnavailable, ReasonRegistryPreparing, "the image repository of this org is being created, which takes a minute or two: ask again shortly")
	}
	login, err := s.registry.PushLogin(ctx, orgID)
	if err != nil {
		return nil, s.registryUnavailable("push", err)
	}
	return &platformapi.RegistryCredential{Registry: login.Registry, Username: login.Username, Password: login.Password, ExpiresAt: login.ExpiresAt,
		Repository: s.registry.Repository(orgID, id)}, nil
}

// ProvideRegistries provisions the repository of every org whose push login was asked for and
// that has none yet, and with every the provisioned ones again, which brings them to what this
// release provisions. It records each. One that fails is logged and tried again by the next pass,
// or for a provisioned one by the next with every.
func (s *Service) ProvideRegistries(ctx context.Context, every bool) error {
	pending, err := s.orgRegistries.OrgRegistriesToProvision(ctx, every)
	if err != nil {
		return fmt.Errorf("platform_experiments.ProvideRegistries: %w", err)
	}
	for _, orgID := range pending {
		log := s.logger.With(zap.String("org_id", orgID))
		failure := ""
		if err := s.registry.Provision(ctx, orgID); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			log.Warn("provide_registries: not provisioned, tried again next pass", zap.Error(err))
			failure = err.Error()
		}
		if err := s.orgRegistries.RecordOrgRegistry(ctx, orgID, failure); err != nil {
			log.Error("provide_registries: record it", zap.Error(err))
			continue
		}
		if failure == "" {
			log.Info("provide_registries: provisioned")
		}
	}
	return nil
}

// StartRegistryPass runs ProvideRegistries when a push login wakes it and every interval, until
// ctx is cancelled, each pass bounded by passTimeout. Until one pass has read the orgs, each
// provisions every org, so a repository provisioned by an earlier release gets what this one
// provisions.
func (s *Service) StartRegistryPass(ctx context.Context, interval, passTimeout time.Duration) {
	if s.registry == nil || s.orgRegistries == nil {
		panic("quota: StartRegistryPass without WithImages: no org would get its repository")
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	every := true
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.registryWake:
		case <-ticker.C:
		}
		passCtx, cancel := context.WithTimeout(ctx, passTimeout)
		if err := s.ProvideRegistries(passCtx, every); err != nil {
			s.logger.Error("provide_registries: scan", zap.Error(err))
		} else {
			every = false
		}
		cancel()
	}
}
