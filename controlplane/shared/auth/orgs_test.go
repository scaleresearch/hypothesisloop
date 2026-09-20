package auth_test

import (
	"context"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// One org with an owner, a member who is no owner, and a cluster; a second org beside it.
func orgsWithAMember() *auth.Access {
	return auth.NewAccess(authtest.Placement{
		Clusters: map[string]string{"cluster-a": "org-a", "cluster-b": "org-b"},
		Projects: map[string]domain.Project{"prj-a": {ID: "prj-a", OrgID: "org-a"}, "prj-b": {ID: "prj-b", OrgID: "org-b"}},
		Members:  map[string]bool{"org-a/u-member": true},
	})
}

var accountCallers = map[string]auth.Principal{
	"owner":       {Kind: auth.KindUser, UserID: "u-owner", OrgIDs: []string{"org-a"}},
	"member":      {Kind: auth.KindUser, UserID: "u-member", OrgIDs: []string{"org-a"}},
	"outsider":    {Kind: auth.KindUser, UserID: "u-other", OrgIDs: []string{"org-b"}},
	"org key":     {Kind: auth.KindKey, KeyID: "k", OrgIDs: []string{"org-a"}},
	"project key": {Kind: auth.KindKey, KeyID: "k2", OrgIDs: []string{"org-a"}, ProjectID: "prj-a"},
	"agent":       {Kind: auth.KindAgent, AgentID: "agent-a", PlatformExperimentID: "pe-a", ProjectID: "prj-a", OrgIDs: []string{"org-a"}},
	"coordinator": {Kind: auth.KindCoordinator, PlatformExperimentID: "pe-a", ProjectID: "prj-a", OrgIDs: []string{"org-a"}},
	"job":         {Kind: auth.KindJob, ExperimentID: "job-a", PlatformExperimentID: "pe-a", ProjectID: "prj-a", OrgIDs: []string{"org-a"}},
	"runtime":     {Kind: auth.KindRuntime, ClusterName: "cluster-a", OrgIDs: []string{"org-a"}},
	"launcher":    {Kind: auth.KindLauncher, LauncherName: "main"},
	"staff":       {Kind: auth.KindStaff},
}

// Every rule of the file, for every kind of caller, in one table: the status each gets.
func TestWhoReadsAndWhoAdministersAnOrgAndItsClusters(t *testing.T) {
	access := orgsWithAMember()
	rules := map[string]func(context.Context) error{
		"hold an account":                    func(ctx context.Context) error { _, err := access.Account(ctx); return err },
		"read org a":                         func(ctx context.Context) error { _, err := access.OrgMember(ctx, "org-a"); return err },
		"read an org that does not exist":    func(ctx context.Context) error { _, err := access.OrgMember(ctx, "org-missing"); return err },
		"administer org a":                   func(ctx context.Context) error { _, err := access.OrgAdmin(ctx, "org-a"); return err },
		"administer an org that is missing":  func(ctx context.Context) error { _, err := access.OrgAdmin(ctx, "org-missing"); return err },
		"administer cluster-a by name":       func(ctx context.Context) error { return access.Cluster(ctx, "cluster-a") },
		"administer cluster-a under org a":   func(ctx context.Context) error { _, err := access.ClusterOf(ctx, "org-a", "cluster-a"); return err },
		"administer cluster-b under org a":   func(ctx context.Context) error { _, err := access.ClusterOf(ctx, "org-a", "cluster-b"); return err },
		"administer no cluster under org a":  func(ctx context.Context) error { _, err := access.ClusterOf(ctx, "org-a", "cluster-none"); return err },
		"administer cluster-a under org b":   func(ctx context.Context) error { _, err := access.ClusterOf(ctx, "org-b", "cluster-a"); return err },
		"administer an unregistered cluster": func(ctx context.Context) error { return access.Cluster(ctx, "cluster-none") },
	}
	//                                            owner member outsider org-key prj-key agent coord job runtime launcher staff
	want := map[string][]int{
		"hold an account":                    {200, 200, 200, 200, 200, 403, 403, 403, 403, 403, 200},
		"read org a":                         {200, 200, 404, 200, 200, 403, 403, 403, 403, 403, 200},
		"read an org that does not exist":    {404, 404, 404, 404, 404, 403, 403, 403, 403, 403, 404},
		"administer org a":                   {200, 403, 404, 200, 403, 403, 403, 403, 403, 403, 200},
		"administer an org that is missing":  {404, 404, 404, 404, 404, 403, 403, 403, 403, 403, 404},
		"administer cluster-a by name":       {200, 403, 404, 200, 404, 404, 404, 404, 403, 404, 200},
		"administer cluster-a under org a":   {200, 403, 404, 200, 403, 403, 403, 403, 403, 403, 200},
		"administer cluster-b under org a":   {404, 403, 404, 404, 403, 403, 403, 403, 403, 403, 404},
		"administer no cluster under org a":  {404, 403, 404, 404, 403, 403, 403, 403, 403, 403, 404},
		"administer cluster-a under org b":   {404, 404, 404, 404, 404, 403, 403, 403, 403, 403, 404},
		"administer an unregistered cluster": {404, 404, 404, 404, 404, 404, 404, 404, 404, 404, 200},
	}
	order := []string{"owner", "member", "outsider", "org key", "project key", "agent", "coordinator", "job", "runtime", "launcher", "staff"}
	if len(want) != len(rules) {
		t.Fatalf("the table holds %d rules and %d are defined", len(want), len(rules))
	}
	for rule, call := range rules {
		for i, caller := range order {
			ctx := auth.WithPrincipal(context.Background(), accountCallers[caller])
			if got := status(call(ctx)); got != want[rule][i] {
				t.Errorf("%s as %s: got = %d, want = %d", rule, caller, got, want[rule][i])
			}
		}
	}
	if got := status(func() error { _, err := access.Account(context.Background()); return err }()); got != 401 {
		t.Errorf("a request with no principal: got = %d, want = 401", got)
	}
}

func TestACallerSeesTheClustersItsJobsRunOnAndKnowsOnlyItsOwn(t *testing.T) {
	own := domain.Cluster{Name: "own", OrgID: "org-a"}
	shared := domain.Cluster{Name: "shared", OrgID: "org-b", Shared: true}
	foreign := domain.Cluster{Name: "foreign", OrgID: "org-b"}
	for caller, want := range map[string][6]bool{
		//             own: placed, known   shared: placed, known   foreign: placed, known
		"owner":    {true, true, true, false, false, false},
		"org key":  {true, true, true, false, false, false},
		"outsider": {false, false, true, true, true, true},
		"launcher": {false, false, false, false, false, false},
		"staff":    {true, true, true, true, true, true},
	} {
		p := accountCallers[caller]
		got := [6]bool{p.PlacedOn(own), p.KnowsCluster(own), p.PlacedOn(shared), p.KnowsCluster(shared), p.PlacedOn(foreign), p.KnowsCluster(foreign)}
		if got != want {
			t.Errorf("%s: placed on and knows (own, shared, another org's) = %v, want %v", caller, got, want)
		}
	}
}
