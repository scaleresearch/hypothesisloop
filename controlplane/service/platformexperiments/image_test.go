package platformexperiments

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/contract/platformapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/platformexperiments/platformexperimentstest"
)

type imageStore struct {
	fakeStore
	pe *domain.PlatformExperiment
	// closesFirst closes the platform experiment between its read and the write of its image.
	closesFirst bool
	recorded    *platformapi.ExperimentImage
}

func (s *imageStore) GetPlatformExperiment(_ context.Context, id string) (*domain.PlatformExperiment, error) {
	if id != s.pe.ID {
		return nil, nil
	}
	return s.pe, nil
}

func (s *imageStore) SetPlatformExperimentImage(_ context.Context, _ string, image domain.ExperimentImage) (bool, error) {
	if s.closesFirst {
		return false, nil
	}
	s.recorded = &image
	return true, nil
}

// projectsOf says project-a is of org-a.
type projectsOf struct{}

func (projectsOf) RegisteredClusters(context.Context) (map[string]domain.Cluster, error) {
	return nil, nil
}
func (projectsOf) PlatformComputeHeld(context.Context, string, string) (domain.ComputeLimit, float64, float64, error) {
	return domain.ComputeLimit{}, 0, 0, nil
}

func (projectsOf) ProjectOrg(_ context.Context, projectID string) (string, error) {
	return strings.Replace(projectID, "project", "org", 1), nil
}

// orgRegistry keeps each org's images under registry.test/hl-<org>/.
type orgRegistry struct {
	pulls, pushes, provisions []string
	failing                   map[string]bool
	// down fails every login, as the registry's provider does in an outage.
	down bool
}

func (*orgRegistry) Repository(orgID, peID string) string {
	return "registry.test/hl-" + orgID + "/" + peID
}
func (*orgRegistry) Holds(orgID, image string) bool {
	return strings.HasPrefix(image, "registry.test/hl-"+orgID+"/")
}
func (*orgRegistry) Managed(image string) bool { return strings.HasPrefix(image, "registry.test/hl-") }
func (r *orgRegistry) Provision(_ context.Context, orgID string) error {
	r.provisions = append(r.provisions, orgID)
	if r.failing[orgID] {
		return errors.New("iam: service account not known yet")
	}
	return nil
}
func (r *orgRegistry) PushLogin(_ context.Context, orgID string) (domain.RegistryLogin, error) {
	if r.down {
		return domain.RegistryLogin{}, errors.New("googleapi: Error 503: backend unavailable")
	}
	r.pushes = append(r.pushes, orgID)
	return domain.RegistryLogin{Registry: "registry.test", Username: "pusher-" + orgID, Password: "push"}, nil
}
func (r *orgRegistry) PullLogin(_ context.Context, orgID, image string) (*domain.RegistryLogin, error) {
	if !r.Holds(orgID, image) {
		return nil, nil
	}
	if r.down {
		return nil, errors.New("googleapi: Error 503: backend unavailable")
	}
	r.pulls = append(r.pulls, orgID)
	return &domain.RegistryLogin{Registry: "registry.test", Username: "puller-" + orgID, Password: "pull"}, nil
}

// orgRegistries keeps which orgs' repositories are wanted and which are provisioned.
type orgRegistries struct {
	wanted      []string
	provisioned map[string]bool
	failures    map[string]string
}

func newOrgRegistries() *orgRegistries {
	return &orgRegistries{provisioned: map[string]bool{}, failures: map[string]string{}}
}

func (o *orgRegistries) AskOrgRegistry(_ context.Context, orgID string) (bool, error) {
	if !slices.Contains(o.wanted, orgID) {
		o.wanted = append(o.wanted, orgID)
	}
	return o.provisioned[orgID], nil
}
func (o *orgRegistries) OrgRegistriesToProvision(context.Context) ([]string, error) {
	var pending []string
	for _, orgID := range o.wanted {
		if !o.provisioned[orgID] {
			pending = append(pending, orgID)
		}
	}
	return pending, nil
}
func (o *orgRegistries) RecordOrgRegistry(_ context.Context, orgID, failure string) error {
	o.failures[orgID] = failure
	o.provisioned[orgID] = failure == ""
	return nil
}

// images answers for the references it holds and records the login each was read with.
type images struct {
	held  map[string]domain.ResolvedImage
	reads map[string]*domain.RegistryLogin
}

func (f *images) Resolve(_ context.Context, ref string, login *domain.RegistryLogin) (domain.ResolvedImage, error) {
	f.reads[ref] = login
	image, ok := f.held[ref]
	if !ok {
		return domain.ResolvedImage{}, errors.New("manifest unknown")
	}
	return image, nil
}

const (
	ownImage    = "registry.test/hl-org-a/pe-1:abc1234-base"
	otherImage  = "registry.test/hl-org-b/pe-9:v1"
	publicMulti = "docker.io/library/python:3.12"
)

type imageFixture struct {
	svc      *Service
	store    *imageStore
	registry *orgRegistry
	orgs     *orgRegistries
	images   *images
}

func newImageFixture(status domain.PlatformExperimentStatus, registry *orgRegistry) imageFixture {
	store := &imageStore{pe: &domain.PlatformExperiment{ID: "pe-1", ProjectID: "project-a", Status: status}}
	held := &images{reads: map[string]*domain.RegistryLogin{}, held: map[string]domain.ResolvedImage{
		ownImage:    {Pinned: "registry.test/hl-org-a/pe-1@sha256:own", Architectures: []string{"amd64"}},
		otherImage:  {Pinned: "registry.test/hl-org-b/pe-9@sha256:other", Architectures: []string{"amd64"}},
		publicMulti: {Pinned: "docker.io/library/python@sha256:index", Architectures: []string{"amd64", "arm64"}},
	}}
	svc := NewService(store, platformexperimentstest.Settings, zap.NewNop(), nil, time.Minute).WithLiveCapacity(time.Minute, projectsOf{}, nil)
	orgs := newOrgRegistries()
	if registry != nil {
		svc.WithImages(registry, orgs, held)
	} else {
		svc.WithImages(nil, orgs, held)
	}
	return imageFixture{svc: svc, store: store, registry: registry, orgs: orgs, images: held}
}

func refusedWith(t *testing.T, err error, status int, reason string) {
	t.Helper()
	var r *refusal.Refusal
	if !errors.As(err, &r) || r.GetStatus() != status || r.Reason != reason {
		t.Fatalf("got %v, want %d %s", err, status, reason)
	}
}

// An image of the org's own repository is read with the org's pull login and recorded by digest
// with the one architecture it runs on.
func TestAnImageOfTheOrgsRepositoryIsReadWithItsPullLoginAndRecordedByDigest(t *testing.T) {
	f := newImageFixture(domain.PlatformExpRunning, &orgRegistry{})
	got, err := f.svc.SetImage(context.Background(), "pe-1", ownImage, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Image != "registry.test/hl-org-a/pe-1@sha256:own" || got.Architecture != "amd64" || f.store.recorded == nil || *f.store.recorded != *got {
		t.Fatalf("recorded %+v, answered %+v, want the digest on amd64", f.store.recorded, got)
	}
	if login := f.images.reads[ownImage]; login == nil || login.Username != "puller-org-a" {
		t.Fatalf("read with %+v, want org-a's pull login", login)
	}
}

// An image in another org's repository is refused before it is read, and nothing is recorded:
// the platform's pull login would otherwise let one org run another's image.
func TestAnImageInAnotherOrgsRepositoryIsRefused(t *testing.T) {
	f := newImageFixture(domain.PlatformExpOpen, &orgRegistry{})
	_, err := f.svc.SetImage(context.Background(), "pe-1", otherImage, "")
	refusedWith(t, err, http.StatusUnprocessableEntity, ReasonImageOfAnotherOrg)
	if f.store.recorded != nil || len(f.registry.pulls) != 0 || len(f.images.reads) != 0 {
		t.Fatalf("recorded %+v after %d pull logins and reads %v", f.store.recorded, len(f.registry.pulls), f.images.reads)
	}
	if !strings.Contains(err.Error(), "registry.test/hl-org-a/pe-1") {
		t.Errorf("refusal %q does not name the org's own repository", err)
	}
}

// An image that runs on several architectures is recorded only with the one its jobs run on,
// which must be one of them. A public one is read without a login.
func TestAMultiArchitectureImageIsRecordedOnlyWithAnArchitectureItRunsOn(t *testing.T) {
	f := newImageFixture(domain.PlatformExpRunning, &orgRegistry{})
	_, err := f.svc.SetImage(context.Background(), "pe-1", publicMulti, "")
	refusedWith(t, err, http.StatusUnprocessableEntity, ReasonImageArchitecture)
	if !strings.Contains(err.Error(), "amd64, arm64") || f.store.recorded != nil {
		t.Fatalf("refusal %q, recorded %+v, want both architectures named and nothing recorded", err, f.store.recorded)
	}
	_, err = f.svc.SetImage(context.Background(), "pe-1", publicMulti, "riscv64")
	refusedWith(t, err, http.StatusUnprocessableEntity, ReasonImageArchitecture)

	got, err := f.svc.SetImage(context.Background(), "pe-1", publicMulti, "arm64")
	if err != nil || got.Architecture != "arm64" || got.Image != "docker.io/library/python@sha256:index" {
		t.Fatalf("with arm64: %+v, %v", got, err)
	}
	if login, read := f.images.reads[publicMulti]; !read || login != nil || len(f.registry.pulls) != 0 {
		t.Fatalf("a public image was read with %+v", login)
	}
}

func TestAnImageItsRegistryDoesNotAnswerForIsRefused(t *testing.T) {
	f := newImageFixture(domain.PlatformExpRunning, nil)
	_, err := f.svc.SetImage(context.Background(), "pe-1", "registry.test/hl-org-a/pe-1:never-pushed", "")
	refusedWith(t, err, http.StatusUnprocessableEntity, domain.ReasonImageUnresolved)
}

// The image is recorded while the platform experiment is open or running: one that is closed, or
// closes between the read and the write, is refused with 409.
func TestAClosedPlatformExperimentRecordsNoImage(t *testing.T) {
	for _, status := range []domain.PlatformExperimentStatus{domain.PlatformExpClosed, domain.PlatformExpDraft} {
		f := newImageFixture(status, &orgRegistry{})
		_, err := f.svc.SetImage(context.Background(), "pe-1", ownImage, "")
		refusedWith(t, err, http.StatusConflict, platformapi.ReasonExperimentClosed)
		if len(f.images.reads) != 0 {
			t.Errorf("%s: the image was read", status)
		}
	}
	f := newImageFixture(domain.PlatformExpRunning, &orgRegistry{})
	f.store.closesFirst = true
	_, err := f.svc.SetImage(context.Background(), "pe-1", ownImage, "")
	refusedWith(t, err, http.StatusConflict, platformapi.ReasonExperimentClosed)

	_, err = f.svc.SetImage(context.Background(), "pe-404", ownImage, "")
	refusedWith(t, err, http.StatusNotFound, "not_found")
}

// A push login is of the org's repository and names where the platform experiment's images go.
// A deployment without a registry hands out none, and a closed platform experiment gets none.
func TestRegistryCredentialsArePushLoginsOfTheOrgsRepository(t *testing.T) {
	f := newImageFixture(domain.PlatformExpOpen, &orgRegistry{})
	f.orgs.provisioned["org-a"] = true
	got, err := f.svc.RegistryCredential(context.Background(), "pe-1")
	if err != nil || got.Username != "pusher-org-a" || got.Repository != "registry.test/hl-org-a/pe-1" {
		t.Fatalf("credential = %+v, %v, want org-a's push login to its repository", got, err)
	}

	_, err = newImageFixture(domain.PlatformExpRunning, nil).svc.RegistryCredential(context.Background(), "pe-1")
	refusedWith(t, err, http.StatusConflict, ReasonNoRegistry)

	closed := newImageFixture(domain.PlatformExpClosed, &orgRegistry{})
	_, err = closed.svc.RegistryCredential(context.Background(), "pe-1")
	refusedWith(t, err, http.StatusConflict, platformapi.ReasonExperimentClosed)
	if len(closed.registry.pushes) != 0 {
		t.Fatal("a closed platform experiment was handed a push login")
	}
}

// The first push login of an org asks for its repository and is answered 503 registry_preparing
// without a call to the registry, which the registry pass provisions off the request. A failed
// provisioning is tried again by the next pass, and once one succeeds a push login only mints.
func TestAnOrgsRepositoryIsProvisionedOnceByThePassOffTheRequest(t *testing.T) {
	ctx := context.Background()
	registry := &orgRegistry{failing: map[string]bool{"org-a": true}}
	f := newImageFixture(domain.PlatformExpRunning, registry)

	_, err := f.svc.RegistryCredential(ctx, "pe-1")
	refusedWith(t, err, http.StatusServiceUnavailable, ReasonRegistryPreparing)
	if len(registry.provisions) != 0 || len(registry.pushes) != 0 || !slices.Equal(f.orgs.wanted, []string{"org-a"}) {
		t.Fatalf("the request provisioned %v and pushed %v, wanted %v", registry.provisions, registry.pushes, f.orgs.wanted)
	}
	select {
	case <-f.svc.registryWake:
	default:
		t.Fatal("the request did not wake the registry pass")
	}

	if err := f.svc.ProvideRegistries(ctx); err != nil {
		t.Fatal(err)
	}
	if f.orgs.provisioned["org-a"] || f.orgs.failures["org-a"] == "" {
		t.Fatalf("a failed provisioning recorded provisioned %v, failure %q", f.orgs.provisioned["org-a"], f.orgs.failures["org-a"])
	}
	_, err = f.svc.RegistryCredential(ctx, "pe-1")
	refusedWith(t, err, http.StatusServiceUnavailable, ReasonRegistryPreparing)

	registry.failing["org-a"] = false
	if err := f.svc.ProvideRegistries(ctx); err != nil {
		t.Fatal(err)
	}
	if !f.orgs.provisioned["org-a"] {
		t.Fatal("the next pass did not provision the org")
	}
	got, err := f.svc.RegistryCredential(ctx, "pe-1")
	if err != nil || got.Username != "pusher-org-a" {
		t.Fatalf("credential = %+v, %v, want org-a's push login", got, err)
	}
	if err := f.svc.ProvideRegistries(ctx); err != nil {
		t.Fatal(err)
	}
	if len(registry.provisions) != 2 {
		t.Fatalf("provisioned %v, want org-a twice and never again once done", registry.provisions)
	}
}

// A login the registry's provider fails to issue is answered 503 registry_unavailable, which a
// caller waits out and asks again, and never names what the provider said.
func TestALoginTheRegistryFailsToIssueIsAnsweredRetryable(t *testing.T) {
	f := newImageFixture(domain.PlatformExpRunning, &orgRegistry{down: true})
	f.orgs.provisioned["org-a"] = true
	_, err := f.svc.RegistryCredential(context.Background(), "pe-1")
	refusedWith(t, err, http.StatusServiceUnavailable, ReasonRegistryUnavailable)
	if strings.Contains(err.Error(), "googleapi") {
		t.Errorf("refusal %q names what the provider said", err)
	}
	_, err = f.svc.SetImage(context.Background(), "pe-1", ownImage, "")
	refusedWith(t, err, http.StatusServiceUnavailable, ReasonRegistryUnavailable)
}
