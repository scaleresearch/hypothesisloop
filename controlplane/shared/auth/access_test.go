package auth_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

var (
	inA  = domain.Tenancy{PlatformExperimentID: "pe-a", ProjectID: "prj-a", OrgID: "org-a"}
	inA2 = domain.Tenancy{PlatformExperimentID: "pe-a2", ProjectID: "prj-a2", OrgID: "org-a"}
	inB  = domain.Tenancy{PlatformExperimentID: "pe-b", ProjectID: "prj-b", OrgID: "org-b"}
)

func twoOrgs() *auth.Access {
	return auth.NewAccess(authtest.Placement{
		PlatformExperiments: map[string]domain.Tenancy{"pe-a": inA, "pe-a2": inA2, "pe-b": inB},
		Experiments:         map[string]domain.Tenancy{"job-a": inA, "job-a2": inA2, "job-b": inB},
		Donations:           map[string]domain.Tenancy{"don-a": inA, "don-b": inB},
		Agents:              map[string]string{"agent-a": "org-a", "agent-a-other": "org-a", "agent-b": "org-b"},
		Clusters:            map[string]string{"cluster-a": "org-a", "cluster-b": "org-b"},
		Projects: map[string]domain.Project{
			"prj-a": {ID: "prj-a", OrgID: "org-a"}, "prj-a2": {ID: "prj-a2", OrgID: "org-a"}, "prj-b": {ID: "prj-b", OrgID: "org-b"},
		},
	})
}

var callers = map[string]auth.Principal{
	"user of A":            {Kind: auth.KindUser, UserID: "u", OrgIDs: []string{"org-a"}},
	"user of A and B":      {Kind: auth.KindUser, UserID: "u2", OrgIDs: []string{"org-a", "org-b"}},
	"key of A":             {Kind: auth.KindKey, OrgIDs: []string{"org-a"}},
	"key of A's project a": {Kind: auth.KindKey, OrgIDs: []string{"org-a"}, ProjectID: "prj-a"},
	"agent in pe-a":        {Kind: auth.KindAgent, AgentID: "agent-a", PlatformExperimentID: "pe-a", ProjectID: "prj-a", OrgIDs: []string{"org-a"}},
	"coordinator of pe-a":  {Kind: auth.KindCoordinator, PlatformExperimentID: "pe-a", ProjectID: "prj-a", OrgIDs: []string{"org-a"}},
	"job job-a":            {Kind: auth.KindJob, ExperimentID: "job-a", PlatformExperimentID: "pe-a", ProjectID: "prj-a", OrgIDs: []string{"org-a"}},
	"staff":                {Kind: auth.KindStaff},
	"user with no org":     {Kind: auth.KindUser, UserID: "u3"},
}

func status(err error) int {
	if err == nil {
		return http.StatusOK
	}
	var refusal *auth.Error
	if errors.As(err, &refusal) {
		return refusal.GetStatus()
	}
	return -1
}

type check struct {
	caller   string
	resource string
	want     int
}

func runChecks(t *testing.T, what string, call func(context.Context, string) error, checks []check) {
	t.Helper()
	for _, c := range checks {
		ctx := auth.WithPrincipal(context.Background(), callers[c.caller])
		if got := status(call(ctx, c.resource)); got != c.want {
			t.Errorf("%s: %s on %s: got = %d, want = %d", what, c.caller, c.resource, got, c.want)
		}
	}
}

func TestPlatformExperimentAccess(t *testing.T) {
	runChecks(t, "platform experiment", twoOrgs().PlatformExperiment, []check{
		{"user of A", "pe-a", 200}, {"user of A", "pe-a2", 200}, {"user of A", "pe-b", 404},
		{"user of A and B", "pe-b", 200},
		{"key of A", "pe-a2", 200}, {"key of A", "pe-b", 404},
		{"key of A's project a", "pe-a", 200}, {"key of A's project a", "pe-a2", 404}, {"key of A's project a", "pe-b", 404},
		{"agent in pe-a", "pe-a", 200}, {"agent in pe-a", "pe-a2", 404}, {"agent in pe-a", "pe-b", 404},
		{"coordinator of pe-a", "pe-a", 200}, {"coordinator of pe-a", "pe-a2", 404}, {"coordinator of pe-a", "pe-b", 404},
		{"staff", "pe-a", 200}, {"staff", "pe-b", 200},
		{"user with no org", "pe-a", 404},
		{"user of A", "pe-missing", 200},
	})
}

func TestExperimentAccess(t *testing.T) {
	runChecks(t, "job", twoOrgs().Experiment, []check{
		{"user of A", "job-a", 200}, {"user of A", "job-b", 404},
		{"key of A's project a", "job-a2", 404},
		{"agent in pe-a", "job-a", 200}, {"agent in pe-a", "job-a2", 404}, {"agent in pe-a", "job-b", 404},
		{"coordinator of pe-a", "job-a2", 404},
		{"job job-a", "job-a", 200}, {"job job-a", "job-a2", 403}, {"job job-a", "job-b", 403}, {"job job-a", "job-missing", 403},
		{"staff", "job-b", 200},
	})
}

func TestDonationAccess(t *testing.T) {
	runChecks(t, "donation", twoOrgs().Donation, []check{
		{"user of A", "don-a", 200}, {"user of A", "don-b", 404},
		{"agent in pe-a", "don-a", 200}, {"agent in pe-a", "don-b", 404},
		{"staff", "don-b", 200},
	})
}

func TestNamingAnAgent(t *testing.T) {
	access := twoOrgs()
	runChecks(t, "agent", access.Agent, []check{
		{"agent in pe-a", "agent-a", 200}, {"agent in pe-a", "agent-a-other", 403}, {"agent in pe-a", "agent-b", 403},
		{"user of A", "agent-a", 200}, {"user of A", "agent-a-other", 200}, {"user of A", "agent-b", 404},
		{"key of A", "agent-b", 404},
		{"coordinator of pe-a", "agent-a-other", 200}, {"coordinator of pe-a", "agent-b", 404},
		{"staff", "agent-b", 200},
		{"user of A", "agent-unregistered", 200},
	})
	runChecks(t, "self", access.Self, []check{
		{"agent in pe-a", "agent-a", 200}, {"agent in pe-a", "agent-new", 403},
		{"user of A", "agent-b", 200},
	})
}

func TestClusterAccess(t *testing.T) {
	runChecks(t, "cluster", twoOrgs().Cluster, []check{
		{"user of A", "cluster-a", 200}, {"user of A", "cluster-b", 404}, {"user of A", "cluster-unregistered", 404},
		{"staff", "cluster-b", 200}, {"staff", "cluster-unregistered", 200},
	})
}

func TestProjectAccess(t *testing.T) {
	runChecks(t, "project", twoOrgs().Project, []check{
		{"user of A", "prj-a", 200}, {"user of A", "prj-b", 404}, {"user of A", "prj-missing", 404},
		{"key of A's project a", "prj-a", 200}, {"key of A's project a", "prj-a2", 404},
		{"coordinator of pe-a", "prj-a", 200}, {"coordinator of pe-a", "prj-a2", 404},
		{"staff", "prj-b", 200}, {"staff", "prj-missing", 404},
	})
}

func TestOrgOfANewAgent(t *testing.T) {
	access := twoOrgs()
	for _, tc := range []struct {
		caller, requested, want string
		status                  int
	}{
		{"user of A", "", "org-a", 200},
		{"user of A", "org-a", "org-a", 200},
		{"user of A", "org-b", "", 404},
		{"user of A and B", "", "", 400},
		{"user of A and B", "org-b", "org-b", 200},
		{"agent in pe-a", "", "org-a", 200},
		{"agent in pe-a", "org-b", "", 404},
		{"staff", "", "", 400},
		{"staff", "org-b", "org-b", 200},
		{"user with no org", "", "", 400},
	} {
		got, err := access.Org(auth.WithPrincipal(context.Background(), callers[tc.caller]), tc.requested)
		if status(err) != tc.status || got != tc.want {
			t.Errorf("%s asking for %q: got = %q, %d, want = %q, %d", tc.caller, tc.requested, got, status(err), tc.want, tc.status)
		}
	}
}

func TestListScopeNarrowsButNeverWidens(t *testing.T) {
	access := twoOrgs()
	for _, tc := range []struct {
		caller, project string
		status          int
		visible         []string
		restricted      bool
	}{
		{"user of A", "", 200, []string{"pe-a", "pe-a2"}, true},
		{"user of A", "prj-a2", 200, []string{"pe-a2"}, true},
		{"user of A", "prj-b", 200, []string{}, true},
		{"user of A and B", "", 200, []string{"pe-a", "pe-a2", "pe-b"}, true},
		{"key of A's project a", "", 200, []string{"pe-a"}, true},
		{"key of A's project a", "prj-a2", 404, nil, false},
		{"agent in pe-a", "", 200, []string{"pe-a"}, true},
		{"coordinator of pe-a", "prj-a2", 404, nil, false},
		{"user with no org", "", 200, []string{}, true},
		{"staff", "", 200, nil, false},
		{"staff", "prj-b", 200, []string{"pe-b"}, true},
	} {
		ids, restricted, err := access.PlatformExperimentIDs(auth.WithPrincipal(context.Background(), callers[tc.caller]), tc.project)
		slices.Sort(ids)
		if status(err) != tc.status || restricted != tc.restricted || (tc.status == 200 && tc.restricted && !slices.Equal(ids, tc.visible)) {
			t.Errorf("%s asking for project %q: got = %v restricted=%v %d, want = %v restricted=%v %d",
				tc.caller, tc.project, ids, restricted, status(err), tc.visible, tc.restricted, tc.status)
		}
	}
}

func TestARequestWithNoPrincipalReachesNothing(t *testing.T) {
	access := twoOrgs()
	ctx := context.Background()
	for name, err := range map[string]error{
		"platform experiment": access.PlatformExperiment(ctx, "pe-a"),
		"job":                 access.Experiment(ctx, "job-a"),
		"donation":            access.Donation(ctx, "don-a"),
		"agent":               access.Agent(ctx, "agent-a"),
		"cluster":             access.Cluster(ctx, "cluster-a"),
		"project":             access.Project(ctx, "prj-a"),
	} {
		if status(err) != http.StatusUnauthorized {
			t.Errorf("%s with no principal: got = %d, want = 401", name, status(err))
		}
	}
}

func TestAuthorIsTheProvidersNameForAUserAndTheStatedOneOtherwise(t *testing.T) {
	user := auth.WithPrincipal(context.Background(), auth.Principal{Kind: auth.KindUser, Name: "Ada Lovelace"})
	if got := auth.Author(user, "Mallory"); got != "Ada Lovelace" {
		t.Errorf("user: got = %q, want the name the identity provider states", got)
	}
	key := auth.WithPrincipal(context.Background(), auth.Principal{Kind: auth.KindKey})
	if got := auth.Author(key, "Grace"); got != "Grace" {
		t.Errorf("key: got = %q, want the stated name", got)
	}
}
