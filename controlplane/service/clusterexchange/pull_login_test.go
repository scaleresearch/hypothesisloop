package clusterexchange

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// brokenOrgPulls mints a login for every org's image but one org's, whose registry fails.
type brokenOrgPulls struct{ broken string }

func (p brokenOrgPulls) PullLogin(_ context.Context, orgID, image string) (*domain.RegistryLogin, error) {
	if orgID == p.broken {
		return nil, errors.New("iam: permission denied")
	}
	return &domain.RegistryLogin{Registry: "registry.test", Username: "puller-" + orgID}, nil
}

// A pull login that cannot be minted leaves out only the job not running yet it is for: every
// other job is answered with its own login. A running job's failure fails the answer, since a
// runtime takes down whatever the answer leaves out.
func TestAFailedPullLoginLeavesOutOnlyThatJobNotRunningYet(t *testing.T) {
	h := wireHandler(t)
	h.logger = zap.NewNop()
	h.pulls = brokenOrgPulls{broken: "org-b"}
	exps := []*domain.Experiment{
		{ID: "job-a", OrgID: "org-a", Image: wireImage, Status: domain.StatusRunning, PlatformExperimentID: "pe-1"},
		{ID: "job-b", OrgID: "org-b", Image: wireImage, Status: domain.StatusSubmitted, PlatformExperimentID: "pe-2"},
		{ID: "job-c", OrgID: "org-a", Image: wireImage, Status: domain.StatusSubmitted, PlatformExperimentID: "pe-1"},
	}
	jobs, err := h.desiredJobs(context.Background(), "cluster-1", exps)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 || jobs[0].ID != "job-a" || jobs[1].ID != "job-c" {
		t.Fatalf("answered %d jobs, want job-a and job-c", len(jobs))
	}
	for _, job := range jobs {
		if job.ImagePull == nil || job.ImagePull.Username != "puller-org-a" {
			t.Errorf("%s pulls with %+v, want org-a's login", job.ID, job.ImagePull)
		}
	}

	exps[1].Status = domain.StatusRunning
	if _, err := h.desiredJobs(context.Background(), "cluster-1", exps); !errors.Is(err, errNoPullLogin) {
		t.Fatalf("a running job without its pull login: %v, want the answer failed", err)
	}
}

// A deployment that keeps no registry pulls every image without a login.
func TestWithoutARegistryEveryImageIsPulledWithoutALogin(t *testing.T) {
	job, err := wireHandler(t).desiredJob(context.Background(), &domain.Experiment{ID: "job-a", OrgID: "org-a", Image: wireImage, PlatformExperimentID: "pe-1"})
	if err != nil || job.ImagePull != nil {
		t.Fatalf("job = %+v, %v, want no pull login", job, err)
	}
}
