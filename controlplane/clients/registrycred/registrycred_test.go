package registrycred

import (
	"context"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// orgRepositories keeps each org's images under registry.test/<org>/ and logs anyone in.
type orgRepositories struct{ pulls []string }

func (*orgRepositories) Repository(orgID, peID string) string {
	return "registry.test/" + orgID + "/" + peID
}
func (*orgRepositories) Holds(orgID, image string) bool {
	return strings.HasPrefix(image, "registry.test/"+orgID+"/")
}
func (*orgRepositories) Managed(image string) bool { return strings.HasPrefix(image, "registry.test/") }
func (p *orgRepositories) IssuePush(context.Context, string) (domain.RegistryLogin, error) {
	panic("a job never pushes")
}
func (p *orgRepositories) IssuePull(_ context.Context, orgID string) (domain.RegistryLogin, error) {
	p.pulls = append(p.pulls, orgID)
	return domain.RegistryLogin{Registry: "registry.test", Username: "puller-" + orgID, Password: "token"}, nil
}

// A job pulls an image of its org's repository with that org's pull login, and every other
// image, a public one or one of another org, with none.
func TestAJobPullsWithALoginOnlyFromItsOrgsRepository(t *testing.T) {
	p := &orgRepositories{}
	ctx := context.Background()
	for _, image := range []string{"docker.io/library/python@sha256:1", "registry.test/org-b/pe-2@sha256:2", "ghcr.io/acme/model@sha256:3"} {
		if login, err := PullFor(ctx, p, "org-a", image); login != nil || err != nil {
			t.Errorf("%s: login %+v, %v, want none", image, login, err)
		}
	}
	if len(p.pulls) != 0 {
		t.Fatalf("pull logins issued for %v", p.pulls)
	}
	login, err := PullFor(ctx, p, "org-a", "registry.test/org-a/pe-1@sha256:4")
	if err != nil || login == nil || login.Username != "puller-org-a" {
		t.Fatalf("the org's own image: %+v, %v, want its pull login", login, err)
	}
	// A deployment without a registry pulls every image without a login.
	if login, err := PullFor(ctx, nil, "org-a", "registry.test/org-a/pe-1@sha256:4"); login != nil || err != nil {
		t.Fatalf("no registry: %+v, %v", login, err)
	}
}
