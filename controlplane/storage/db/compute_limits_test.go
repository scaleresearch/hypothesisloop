package db

import (
	"context"
	"sync"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// limitedOrgs is twoOrgs and a cluster of the platform's org, so org A's jobs placed there are on
// the platform's compute, and org A held to limit. Each job is one L40, 0.25 AccH/h.
func limitedOrgs(t *testing.T, limit domain.ComputeLimit) (*twoOrgs, string) {
	t.Helper()
	w := newTwoOrgs(t)
	platform := w.platformCluster(t)
	if err := w.clusters.SetOrgComputeLimit(context.Background(), w.orgA, limit); err != nil {
		t.Fatal(err)
	}
	return w, platform
}

func (w *twoOrgs) claimed(t *testing.T, id, cluster string) *domain.ClaimRefusal {
	t.Helper()
	refused, err := w.exps.ClaimSubmitted(context.Background(), id, cluster, &domain.JobSpec{AcceleratorCount: 1}, nil)
	if err != nil {
		t.Fatalf("ClaimSubmitted(%s, %s): %v", id, cluster, err)
	}
	return refused
}

func TestAClaimOnAPlatformClusterIsHeldToItsProjectsLimit(t *testing.T) {
	w, platform := limitedOrgs(t, domain.ComputeLimit{Org: 1, Project: 0.5})
	for _, id := range []string{"first", "second"} {
		if refused := w.claimed(t, w.queued(t, id, w.peA), platform); refused != nil {
			t.Fatalf("%s within the project's limit: got = %+v, want it placed", id, refused)
		}
	}
	third := w.queued(t, "third", w.peA)
	refused := w.claimed(t, third, platform)
	want := domain.ClaimRefusal{Code: domain.NotAdmittedProjectComputeLimit, Holder: domain.DefaultProjectName, Held: 0.5, Limit: 0.5, Needs: 0.25}
	if refused == nil || *refused != want {
		t.Fatalf("the third on the platform's cluster: got = %+v, want %+v", refused, want)
	}
	if refused := w.claimed(t, third, w.ofA); refused != nil {
		t.Fatalf("the third on its org's own cluster: got = %+v, want it placed", refused)
	}
	limit, orgHeld, projectHeld, err := w.clusters.PlatformComputeHeld(context.Background(), w.orgA, defaultProjectOf(t, w.clusters, w.orgA).ID)
	if err != nil || limit != (domain.ComputeLimit{Org: 1, Project: 0.5}) || orgHeld != 0.5 || projectHeld != 0.5 {
		t.Fatalf("what the org holds: got = %+v %v %v, %v, want 0.5 of the platform's held, its own cluster's not", limit, orgHeld, projectHeld, err)
	}
}

func TestAClaimOnAPlatformClusterIsHeldToItsOrgsLimitAcrossProjects(t *testing.T) {
	w, platform := limitedOrgs(t, domain.ComputeLimit{Org: 0.75, Project: 0.5})
	ctx := context.Background()
	other, err := w.clusters.CreateProject(ctx, w.orgA, "other-"+w.suffix, 0)
	if err != nil {
		t.Fatal(err)
	}
	peOther := "pe-other-" + w.suffix
	createTestPEIn(t, w.pool, peOther, other.ID)
	for _, id := range []string{"a1", "a2"} {
		if refused := w.claimed(t, w.queued(t, id, w.peA), platform); refused != nil {
			t.Fatalf("%s: got = %+v, want it placed", id, refused)
		}
	}
	if refused := w.claimed(t, w.queued(t, "o1", peOther), platform); refused != nil {
		t.Fatalf("the other project's first: got = %+v, want it placed", refused)
	}
	refused := w.claimed(t, w.queued(t, "o2", peOther), platform)
	if refused == nil || refused.Code != domain.NotAdmittedOrgComputeLimit || refused.Held != 0.75 || refused.Limit != 0.75 || refused.Needs != 0.25 {
		t.Fatalf("past the org's limit within the project's: got = %+v, want org_compute_limit holding 0.75 of 0.75", refused)
	}
	if refused := w.claimed(t, w.queued(t, "b1", w.peB), platform); refused != nil {
		t.Fatalf("another org's job: got = %+v, want it placed", refused)
	}

	// Lowering the limit evicts nothing: the placed jobs stay placed and the next waits.
	if err := w.clusters.SetOrgComputeLimit(ctx, w.orgA, domain.ComputeLimit{Org: 0.25, Project: 0.25}); err != nil {
		t.Fatal(err)
	}
	var placed int
	if err := w.pool.pool.QueryRow(ctx, `SELECT count(*) FROM experiments WHERE cluster_name = $1 AND status = 'SUBMITTED' AND acch_per_hour = 0.25`, platform).Scan(&placed); err != nil || placed != 4 {
		t.Fatalf("placed after the limit was lowered: got = %d, %v, want 4", placed, err)
	}
	if err := w.clusters.SetOrgComputeLimit(ctx, w.orgA, domain.ComputeLimit{Org: 0.25, Project: 0.5}); err == nil {
		t.Fatal("a project limit above the org's was accepted")
	}
	if err := w.clusters.SetOrgComputeLimit(ctx, "org-nobody", domain.ComputeLimit{Org: 1, Project: 1}); err != ErrOrgNotFound {
		t.Fatalf("no such org: got = %v, want ErrOrgNotFound", err)
	}
}

// Claims on different platform clusters take different cluster locks; the org's lock serializes
// them, so the project's limit holds however many race.
func TestConcurrentClaimsOnPlatformClustersKeepTheLimit(t *testing.T) {
	w, platform := limitedOrgs(t, domain.ComputeLimit{Org: 1, Project: 0.5})
	ctx := context.Background()
	third := "of-c-" + w.suffix
	if _, err := w.clusters.RegisterCluster(ctx, third, domain.PlatformOrgID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = w.pool.pool.Exec(context.Background(), `DELETE FROM clusters WHERE name = $1`, third) })
	var jobs []string
	for i := range 8 {
		jobs = append(jobs, w.queued(t, "race-"+string(rune('a'+i)), w.peA))
	}
	var wg sync.WaitGroup
	refusals := make([]*domain.ClaimRefusal, len(jobs))
	errs := make([]error, len(jobs))
	for i, id := range jobs {
		cluster := platform
		if i%2 == 1 {
			cluster = third
		}
		wg.Go(func() {
			refusals[i], errs[i] = w.exps.ClaimSubmitted(ctx, id, cluster, &domain.JobSpec{AcceleratorCount: 1}, nil)
		})
	}
	wg.Wait()
	placed := 0
	for i := range jobs {
		switch {
		case errs[i] != nil:
			t.Fatalf("claim %s: %v", jobs[i], errs[i])
		case refusals[i] == nil:
			placed++
		case refusals[i].Code != domain.NotAdmittedProjectComputeLimit:
			t.Errorf("claim %s: got = %+v, want project_compute_limit", jobs[i], refusals[i])
		}
	}
	if placed != 2 {
		t.Fatalf("placed by 8 racing claims under a limit of two jobs: got = %d, want 2", placed)
	}
}
