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

func TestACallerKnowsOnlyTheClustersOfItsOwnOrg(t *testing.T) {
	own := domain.Cluster{Name: "own", OrgID: "org-a"}
	shared := domain.Cluster{Name: "shared", OrgID: "org-b", Shared: true}
	for caller, want := range map[string][2]bool{
		"owner": {true, false}, "org key": {true, false}, "outsider": {false, true}, "launcher": {false, false}, "staff": {true, true},
	} {
		p := accountCallers[caller]
		if got := [2]bool{p.KnowsCluster(own), p.KnowsCluster(shared)}; got != want {
			t.Errorf("%s knows (org a's cluster, org b's shared one) = %v, want %v", caller, got, want)
		}
	}
}

// Capacity is read for one org at a time, so a person in two orgs is never shown one org's
// clusters while looking at the other.
func TestCapacityIsReadForOneOrgTheCallerReaches(t *testing.T) {
	access := auth.NewAccess(authtest.Placement{Clusters: map[string]string{"cluster-a": "org-a", "cluster-b": "org-b"}})
	inBoth := auth.Principal{Kind: auth.KindUser, UserID: "u-both", OrgIDs: []string{"org-a", "org-b"}}
	agent := auth.Principal{Kind: auth.KindAgent, AgentID: "agent-1", OrgIDs: []string{"org-a"}, PlatformExperimentID: "pe-1"}
	for name, tc := range map[string]struct {
		caller    auth.Principal
		requested string
		org       string
		every     bool
		status    int
	}{
		"a person in one org, naming none":     {accountCallers["owner"], "", "org-a", false, 200},
		"a person in one org, naming it":       {accountCallers["owner"], "org-a", "org-a", false, 200},
		"a person in one org, naming another":  {accountCallers["owner"], "org-b", "", false, 404},
		"a person in two orgs, naming none":    {inBoth, "", "", false, 400},
		"a person in two orgs, naming one":     {inBoth, "org-b", "org-b", false, 200},
		"a person in two orgs, naming a third": {inBoth, "org-c", "", false, 404},
		"a key, naming none":                   {accountCallers["org key"], "", "org-a", false, 200},
		"a key, naming another org":            {accountCallers["org key"], "org-b", "", false, 404},
		"an agent, naming none":                {agent, "", "org-a", false, 200},
		"an agent, naming another org":         {agent, "org-b", "", false, 404},
		"staff, naming none":                   {accountCallers["staff"], "", "", true, 200},
		"staff, naming an org":                 {accountCallers["staff"], "org-b", "org-b", false, 200},
		"staff, naming an org that is not one": {accountCallers["staff"], "org-never", "", false, 404},
	} {
		org, every, err := access.PlacementOrg(auth.WithPrincipal(context.Background(), tc.caller), tc.requested)
		if got := status(err); got != tc.status || org != tc.org || every != tc.every {
			t.Errorf("%s: got = (%q, every %v, %d), want = (%q, every %v, %d)", name, org, every, got, tc.org, tc.every, tc.status)
		}
	}
}
