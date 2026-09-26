package auth_test

import (
	"context"
	"slices"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// Org B holds two platform experiments. Staff shared pe-show; pe-b is its own.
func withAShowcase() *auth.Access {
	show := domain.Tenancy{PlatformExperimentID: "pe-show", ProjectID: "prj-b", OrgID: "org-b", Shared: true}
	return auth.NewAccess(authtest.Placement{
		PlatformExperiments: map[string]domain.Tenancy{"pe-a": inA, "pe-b": inB, "pe-show": show},
		Experiments:         map[string]domain.Tenancy{"job-a": by(inA, "agent-a"), "job-b": inB, "job-show": by(show, "agent-b")},
		Donations:           map[string]domain.Tenancy{"don-b": inB, "don-show": by(show, "agent-b")},
		Agents:              map[string]string{"agent-a": "org-a", "agent-b": "org-b"},
		Projects:            map[string]domain.Project{"prj-a": {ID: "prj-a", OrgID: "org-a"}, "prj-b": {ID: "prj-b", OrgID: "org-b"}},
	})
}

// What each caller of org A gets on org B's shared platform experiment and what is placed in it,
// as a read and as anything else, beside the same on org B's own.
func TestASharedPlatformExperimentIsReadByEveryAccountAndWrittenByNoneOutsideItsOrg(t *testing.T) {
	access := withAShowcase()
	rules := map[string]func(context.Context) error{
		"the shared platform experiment":   func(ctx context.Context) error { return access.PlatformExperiment(ctx, "pe-show") },
		"something placed in it":           func(ctx context.Context) error { return access.Within(ctx, "hypothesis", "pe-show") },
		"a job of it":                      func(ctx context.Context) error { return access.Experiment(ctx, "job-show") },
		"a job of it, as its owner":        func(ctx context.Context) error { return access.OwnExperiment(ctx, "job-show") },
		"a donation request of it":         func(ctx context.Context) error { return access.Donation(ctx, "don-show") },
		"org b's own platform experiment":  func(ctx context.Context) error { return access.PlatformExperiment(ctx, "pe-b") },
		"a job of org b's own":             func(ctx context.Context) error { return access.Experiment(ctx, "job-b") },
		"a donation request of org b's":    func(ctx context.Context) error { return access.Donation(ctx, "don-b") },
		"an agent of org b, named":         func(ctx context.Context) error { return access.Agent(ctx, "agent-b") },
		"acting as an agent of org b":      func(ctx context.Context) error { return access.ActAs(ctx, "agent-b") },
		"creating in org b's project":      func(ctx context.Context) error { return access.Project(ctx, "prj-b") },
		"a platform experiment not there":  func(ctx context.Context) error { return access.PlatformExperiment(ctx, "pe-none") },
		"the caller's own, for comparison": func(ctx context.Context) error { return access.PlatformExperiment(ctx, "pe-a") },
	}
	// As a read, then as anything else.
	want := map[string]map[string][2]int{
		"user of A": {
			"the shared platform experiment": {200, 404}, "something placed in it": {200, 404}, "a job of it": {200, 404},
			"a job of it, as its owner": {200, 404}, "a donation request of it": {200, 404},
			"org b's own platform experiment": {404, 404}, "a job of org b's own": {404, 404}, "a donation request of org b's": {404, 404},
			"an agent of org b, named": {404, 404}, "acting as an agent of org b": {404, 404}, "creating in org b's project": {404, 404},
			"a platform experiment not there": {404, 404}, "the caller's own, for comparison": {200, 200},
		},
		"key of A": {
			"the shared platform experiment": {200, 404}, "something placed in it": {200, 404}, "a job of it": {200, 404},
			"a job of it, as its owner": {200, 404}, "a donation request of it": {200, 404},
			"org b's own platform experiment": {404, 404}, "a job of org b's own": {404, 404}, "a donation request of org b's": {404, 404},
			"an agent of org b, named": {404, 404}, "acting as an agent of org b": {404, 404}, "creating in org b's project": {404, 404},
			"a platform experiment not there": {404, 404}, "the caller's own, for comparison": {200, 200},
		},
		"key of A's project a": {
			"the shared platform experiment": {200, 404}, "something placed in it": {200, 404}, "a job of it": {200, 404},
			"a job of it, as its owner": {200, 404}, "a donation request of it": {200, 404},
			"org b's own platform experiment": {404, 404}, "a job of org b's own": {404, 404}, "a donation request of org b's": {404, 404},
			"an agent of org b, named": {404, 404}, "acting as an agent of org b": {404, 404}, "creating in org b's project": {404, 404},
			"a platform experiment not there": {404, 404}, "the caller's own, for comparison": {200, 200},
		},
		"user with no org": {
			"the shared platform experiment": {200, 404}, "something placed in it": {200, 404}, "a job of it": {200, 404},
			"a job of it, as its owner": {200, 404}, "a donation request of it": {200, 404},
			"org b's own platform experiment": {404, 404}, "a job of org b's own": {404, 404}, "a donation request of org b's": {404, 404},
			"an agent of org b, named": {404, 404}, "acting as an agent of org b": {404, 404}, "creating in org b's project": {404, 404},
			"a platform experiment not there": {404, 404}, "the caller's own, for comparison": {404, 404},
		},
		// A token the platform derived stays in its own platform experiment, shared or not.
		"agent in pe-a": {
			"the shared platform experiment": {404, 404}, "something placed in it": {404, 404}, "a job of it": {404, 404},
			"a job of it, as its owner": {404, 404}, "a donation request of it": {404, 404},
			"org b's own platform experiment": {404, 404}, "a job of org b's own": {404, 404}, "a donation request of org b's": {404, 404},
			"an agent of org b, named": {403, 403}, "acting as an agent of org b": {403, 403}, "creating in org b's project": {403, 403},
			"a platform experiment not there": {404, 404}, "the caller's own, for comparison": {200, 200},
		},
		"coordinator of pe-a": {
			"the shared platform experiment": {404, 404}, "something placed in it": {404, 404}, "a job of it": {404, 404},
			"a job of it, as its owner": {404, 404}, "a donation request of it": {404, 404},
			"org b's own platform experiment": {404, 404}, "a job of org b's own": {404, 404}, "a donation request of org b's": {404, 404},
			"an agent of org b, named": {404, 404}, "acting as an agent of org b": {403, 403}, "creating in org b's project": {403, 403},
			"a platform experiment not there": {404, 404}, "the caller's own, for comparison": {200, 200},
		},
		"job job-a": {
			"the shared platform experiment": {404, 404}, "something placed in it": {404, 404}, "a job of it": {403, 403},
			"a job of it, as its owner": {403, 403}, "a donation request of it": {404, 404},
			"org b's own platform experiment": {404, 404}, "a job of org b's own": {403, 403}, "a donation request of org b's": {404, 404},
			"an agent of org b, named": {404, 404}, "acting as an agent of org b": {404, 404}, "creating in org b's project": {403, 403},
			"a platform experiment not there": {404, 404}, "the caller's own, for comparison": {200, 200},
		},
	}
	for caller, cells := range want {
		if len(cells) != len(rules) {
			t.Fatalf("%s: %d cells for %d rules", caller, len(cells), len(rules))
		}
		asked := auth.WithPrincipal(context.Background(), callers[caller])
		for rule, call := range rules {
			got := [2]int{status(call(auth.WithRead(asked, ""))), status(call(asked))}
			if got != cells[rule] {
				t.Errorf("%s, %s: (read, anything else) = %v, want = %v", caller, rule, got, cells[rule])
			}
		}
	}
}

// A listing is the caller's own. Narrowed to a shared platform experiment by a read it is that
// platform experiment's, and the listing of the shared ones is asked for by name.
func TestAListingReachesASharedPlatformExperimentOnlyWhenItIsNamed(t *testing.T) {
	access := withAShowcase()
	user := auth.WithPrincipal(context.Background(), callers["user of A"])
	own := domain.TenantScope{OrgIDs: []string{"org-a"}}
	showcase := domain.TenantScope{AllOrgs: true, PlatformExperimentID: "pe-show"}
	for name, tc := range map[string]struct {
		ctx  context.Context
		want domain.TenantScope
	}{
		"a read naming nothing":                    {auth.WithRead(user, ""), own},
		"a read naming the shared one":             {auth.WithRead(user, "pe-show"), showcase},
		"a read naming org b's own":                {auth.WithRead(user, "pe-b"), own},
		"a read naming the caller's own":           {auth.WithRead(user, "pe-a"), own},
		"a read naming one that is not there":      {auth.WithRead(user, "pe-none"), own},
		"a read naming an id that is a path":       {auth.WithRead(user, "pe-show/../pe-b"), own},
		"a request that is no read":                {user, own},
		"an agent's read naming the shared one":    {auth.WithRead(auth.WithPrincipal(context.Background(), callers["agent in pe-a"]), "pe-show"), callers["agent in pe-a"].Scope()},
		"a project key's read naming the shared 1": {auth.WithRead(auth.WithPrincipal(context.Background(), callers["key of A's project a"]), "pe-show"), showcase},
	} {
		got, err := access.ListScope(tc.ctx, "")
		if err != nil || !sameScope(got, tc.want) {
			t.Errorf("%s: scope = %+v, %v, want = %+v", name, got, err, tc.want)
		}
	}
	ids, restricted, err := access.PlatformExperimentIDs(auth.WithRead(user, "pe-show"), "")
	if err != nil || !restricted || !slices.Equal(ids, []string{"pe-show"}) {
		t.Errorf("platform experiments a listing narrowed to the shared one draws from: got = %v, %v, %v, want only pe-show", ids, restricted, err)
	}
	ids, _, err = access.PlatformExperimentIDs(auth.WithRead(user, ""), "")
	if err != nil || !slices.Equal(ids, []string{"pe-a"}) {
		t.Errorf("platform experiments an unnarrowed listing draws from: got = %v, %v, want only the caller's own pe-a", ids, err)
	}

	// The scope stays the caller's own; the shared ones are listed beside it for an account that reads.
	inProject := own
	inProject.ProjectID = "prj-a"
	for name, tc := range map[string]struct {
		ctx        context.Context
		projectID  string
		want       domain.TenantScope
		withShared bool
	}{
		"an account's listing":                 {auth.WithRead(user, ""), "", own, true},
		"an account's listing of one project":  {auth.WithRead(user, ""), "prj-a", inProject, true},
		"an account's request that is no read": {user, "", own, false},
		"an agent's listing":                   {auth.WithRead(auth.WithPrincipal(context.Background(), callers["agent in pe-a"]), ""), "", callers["agent in pe-a"].Scope(), false},
	} {
		got, withShared, err := access.PlatformExperimentListScope(tc.ctx, tc.projectID)
		if err != nil || !sameScope(got, tc.want) || withShared != tc.withShared {
			t.Errorf("%s: scope = %+v, with shared = %v, %v, want = %+v, %v", name, got, withShared, err, tc.want, tc.withShared)
		}
	}
}

func sameScope(a, b domain.TenantScope) bool {
	return a.AllOrgs == b.AllOrgs && slices.Equal(a.OrgIDs, b.OrgIDs) && a.ProjectID == b.ProjectID && a.PlatformExperimentID == b.PlatformExperimentID
}
