package auth_test

import (
	"context"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// One org with an owner, a member who is no owner, and a cluster; a second org beside it.
func orgsWithAMember() *auth.Access {
	return auth.NewAccess(authtest.Placement{
		Clusters: map[string]string{"cluster-a": "org-a", "cluster-b": "org-b"},
		Projects: map[string]domain.Project{
			"prj-a": {ID: "prj-a", OrgID: "org-a"}, "prj-a2": {ID: "prj-a2", OrgID: "org-a"}, "prj-b": {ID: "prj-b", OrgID: "org-b"},
		},
		Keys: map[string]domain.APIKey{
			"key-a": {ID: "key-a", OrgID: "org-a"}, "key-a-prj": {ID: "key-a-prj", OrgID: "org-a", ProjectID: "prj-a"},
			"key-a-prj2": {ID: "key-a-prj2", OrgID: "org-a", ProjectID: "prj-a2"}, "key-b": {ID: "key-b", OrgID: "org-b"},
			"k": {ID: "k", OrgID: "org-a", Actor: domain.Actor{Kind: domain.ActorUser, ID: "u-owner"}},
		},
		Members: map[string]bool{"org-a/u-member": true},
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
	"staff":       {Kind: auth.KindStaff},
}

// Every rule of the file, for every kind of caller, in one table: the status each gets.
func TestWhoReadsAndWhoAdministersAnOrgAndItsClusters(t *testing.T) {
	access := orgsWithAMember()
	rules := map[string]func(context.Context) error{
		"hold an account":                      func(ctx context.Context) error { _, err := access.Account(ctx); return err },
		"read org a":                           func(ctx context.Context) error { _, err := access.OrgMember(ctx, "org-a"); return err },
		"read an org that does not exist":      func(ctx context.Context) error { _, err := access.OrgMember(ctx, "org-missing"); return err },
		"administer org a":                     func(ctx context.Context) error { _, err := access.OrgAdmin(ctx, "org-a"); return err },
		"administer an org that is missing":    func(ctx context.Context) error { _, err := access.OrgAdmin(ctx, "org-missing"); return err },
		"administer cluster-a under org a":     func(ctx context.Context) error { _, err := access.ClusterOf(ctx, "org-a", "cluster-a"); return err },
		"administer cluster-b under org a":     func(ctx context.Context) error { _, err := access.ClusterOf(ctx, "org-a", "cluster-b"); return err },
		"administer no cluster under org a":    func(ctx context.Context) error { _, err := access.ClusterOf(ctx, "org-a", "cluster-none"); return err },
		"administer cluster-a under org b":     func(ctx context.Context) error { _, err := access.ClusterOf(ctx, "org-b", "cluster-a"); return err },
		"change project prj-a":                 func(ctx context.Context) error { _, err := access.ProjectAdmin(ctx, "prj-a"); return err },
		"change project prj-a2":                func(ctx context.Context) error { _, err := access.ProjectAdmin(ctx, "prj-a2"); return err },
		"change a project that does not exist": func(ctx context.Context) error { _, err := access.ProjectAdmin(ctx, "prj-none"); return err },
		"place the org-wide key of org a":      func(ctx context.Context) error { _, err := access.APIKey(ctx, "key-a"); return err },
		"place a key of project prj-a":         func(ctx context.Context) error { _, err := access.APIKey(ctx, "key-a-prj"); return err },
		"place a key of project prj-a2":        func(ctx context.Context) error { _, err := access.APIKey(ctx, "key-a-prj2"); return err },
		"place a key that does not exist":      func(ctx context.Context) error { _, err := access.APIKey(ctx, "key-none"); return err },
		"be a signed-in person":                func(ctx context.Context) error { _, err := access.Person(ctx); return err },
		"invite into org a":                    func(ctx context.Context) error { _, err := access.Inviter(ctx, "org-a"); return err },
		"invite into an org that is missing":   func(ctx context.Context) error { _, err := access.Inviter(ctx, "org-missing"); return err },
		"end an invite into org a": func(ctx context.Context) error {
			return access.Invite(ctx, &domain.OrgInvite{ID: "inv-a", OrgID: "org-a"})
		},
		"end an invite that does not exist":    func(ctx context.Context) error { return access.Invite(ctx, nil) },
		"remove the owner from org a":          func(ctx context.Context) error { return access.MemberRemoval(ctx, "org-a", "u-owner") },
		"remove the member from org a":         func(ctx context.Context) error { return access.MemberRemoval(ctx, "org-a", "u-member") },
		"remove someone from a missing org":    func(ctx context.Context) error { return access.MemberRemoval(ctx, "org-missing", "u-member") },
		"mint an org-wide key of org a":        func(ctx context.Context) error { _, err := access.KeyMinter(ctx, "org-a", ""); return err },
		"mint a key of project prj-a":          func(ctx context.Context) error { _, err := access.KeyMinter(ctx, "org-a", "prj-a"); return err },
		"mint a key in an org that is missing": func(ctx context.Context) error { _, err := access.KeyMinter(ctx, "org-missing", "prj-a"); return err },
		"end a key the owner made": func(ctx context.Context) error {
			return access.KeyEnd(ctx, &domain.APIKey{OrgID: "org-a", Actor: domain.Actor{Kind: domain.ActorUser, ID: "u-owner"}})
		},
		"end a key the member made": func(ctx context.Context) error {
			return access.KeyEnd(ctx, &domain.APIKey{OrgID: "org-a", ProjectID: "prj-a", Actor: domain.Actor{Kind: domain.ActorUser, ID: "u-member"}})
		},
	}
	//                                            owner member outsider org-key prj-key agent coord job runtime staff
	want := map[string][]int{
		"hold an account":                      {200, 200, 200, 200, 200, 403, 403, 403, 403, 200},
		"read org a":                           {200, 200, 404, 200, 200, 403, 403, 403, 403, 200},
		"read an org that does not exist":      {404, 404, 404, 404, 404, 403, 403, 403, 403, 404},
		"administer org a":                     {200, 403, 404, 200, 403, 403, 403, 403, 403, 200},
		"administer an org that is missing":    {404, 404, 404, 404, 404, 403, 403, 403, 403, 404},
		"administer cluster-a under org a":     {200, 403, 404, 200, 403, 403, 403, 403, 403, 200},
		"administer cluster-b under org a":     {404, 403, 404, 404, 403, 403, 403, 403, 403, 404},
		"administer no cluster under org a":    {404, 403, 404, 404, 403, 403, 403, 403, 403, 404},
		"administer cluster-a under org b":     {404, 404, 404, 404, 404, 403, 403, 403, 403, 404},
		"change project prj-a":                 {200, 403, 404, 200, 403, 403, 403, 403, 403, 200},
		"change project prj-a2":                {200, 403, 404, 200, 404, 403, 403, 403, 403, 200},
		"change a project that does not exist": {404, 404, 404, 404, 404, 403, 403, 403, 403, 404},
		"place the org-wide key of org a":      {200, 200, 404, 200, 404, 403, 403, 403, 403, 200},
		"place a key of project prj-a":         {200, 200, 404, 200, 200, 403, 403, 403, 403, 200},
		"place a key of project prj-a2":        {200, 200, 404, 200, 404, 403, 403, 403, 403, 200},
		"place a key that does not exist":      {404, 404, 404, 404, 404, 403, 403, 403, 403, 404},
		"be a signed-in person":                {200, 200, 200, 403, 403, 403, 403, 403, 403, 403},
		"invite into org a":                    {200, 403, 404, 200, 403, 403, 403, 403, 403, 403},
		"invite into an org that is missing":   {404, 404, 404, 404, 404, 403, 403, 403, 403, 404},
		"end an invite into org a":             {200, 403, 404, 200, 403, 403, 403, 403, 403, 200},
		"end an invite that does not exist":    {404, 404, 404, 404, 404, 403, 403, 403, 403, 404},
		"remove the owner from org a":          {200, 403, 404, 200, 403, 403, 403, 403, 403, 200},
		"remove the member from org a":         {200, 200, 404, 200, 403, 403, 403, 403, 403, 200},
		"remove someone from a missing org":    {404, 404, 404, 404, 404, 403, 403, 403, 403, 404},
		"mint an org-wide key of org a":        {200, 403, 404, 200, 403, 403, 403, 403, 403, 200},
		"mint a key of project prj-a":          {200, 200, 404, 200, 403, 403, 403, 403, 403, 200},
		"mint a key in an org that is missing": {404, 404, 404, 404, 404, 403, 403, 403, 403, 404},
		"end a key the owner made":             {200, 403, 404, 200, 403, 403, 403, 403, 403, 200},
		"end a key the member made":            {200, 200, 404, 200, 403, 403, 403, 403, 403, 200},
	}
	order := []string{"owner", "member", "outsider", "org key", "project key", "agent", "coordinator", "job", "runtime", "staff"}
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
		"owner": {true, false}, "org key": {true, false}, "outsider": {false, true}, "staff": {true, true},
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

// Project and key ids are the store's own and carry an underscore (prj_<uuid>), which the shape
// rule for ids that become paths and object names refuses. They go nowhere but a query parameter.
func TestAProjectAndAKeyAreFoundUnderTheIdsTheStoreIssues(t *testing.T) {
	access := auth.NewAccess(authtest.Placement{
		Projects: map[string]domain.Project{"prj_01a10af4-8f68": {ID: "prj_01a10af4-8f68", OrgID: "org-a"}},
		Keys:     map[string]domain.APIKey{"key_01A10AF4": {ID: "key_01A10AF4", OrgID: "org-a"}},
	})
	ctx := auth.WithPrincipal(context.Background(), accountCallers["owner"])
	if project, err := access.ProjectAdmin(ctx, "prj_01a10af4-8f68"); err != nil || project == nil {
		t.Errorf("a project under the id the store issued: got = %v, %v", project, err)
	}
	if key, err := access.APIKey(ctx, "key_01A10AF4"); err != nil || key == nil {
		t.Errorf("a key under the id the store issued: got = %v, %v", key, err)
	}
}

func TestOnlyStaffAreHeldToNoLimit(t *testing.T) {
	access := orgsWithAMember()
	for caller, want := range map[string]bool{"owner": true, "member": true, "org key": true, "project key": true, "staff": false} {
		if got, err := access.HeldToLimits(auth.WithPrincipal(context.Background(), accountCallers[caller])); err != nil || got != want {
			t.Errorf("%s held to the account limits: got = %v, %v, want = %v", caller, got, err, want)
		}
	}
	if _, err := access.HeldToLimits(auth.WithPrincipal(context.Background(), accountCallers["agent"])); status(err) != 403 {
		t.Errorf("an agent token asking: got = %v, want a 403", err)
	}
}

// An invite is answered for by a person: the owner who makes it, or the one who made the key
// that makes it.
func TestAnInviteIsAnsweredForByAPerson(t *testing.T) {
	access := orgsWithAMember()
	for caller, want := range map[string]string{"owner": "u-owner", "org key": "u-owner"} {
		got, err := access.Inviter(auth.WithPrincipal(context.Background(), accountCallers[caller]), "org-a")
		if err != nil || got != want {
			t.Errorf("inviter as %s: got = %q, %v, want = %q", caller, got, err, want)
		}
	}
	signedInStaff := auth.Principal{Kind: auth.KindStaff, UserID: "u-staff"}
	if got, err := access.Inviter(auth.WithPrincipal(context.Background(), signedInStaff), "org-a"); err != nil || got != "u-staff" {
		t.Errorf("inviter as staff signed in: got = %q, %v, want = u-staff", got, err)
	}
}

// Removing a person's idea is also an owner's of the org it lives in: a signed-in owner of that
// org, and nobody else, owns it in this sense.
func TestOnlyASignedInOwnerOfItsOrgOwnsTheOrgOfAPlatformExperiment(t *testing.T) {
	access := auth.NewAccess(authtest.Placement{
		PlatformExperiments: map[string]domain.Tenancy{"pe-a": {PlatformExperimentID: "pe-a", ProjectID: "prj-a", OrgID: "org-a"}},
		Members:             map[string]bool{"org-a/u-member": true},
	})
	want := map[string]bool{"owner": true, "member": false, "outsider": false, "org key": false, "agent": false, "coordinator": false, "staff": false}
	for name, owns := range want {
		got, err := access.OwnsOrgOf(auth.WithPrincipal(context.Background(), accountCallers[name]), "pe-a")
		if err != nil || got != owns {
			t.Errorf("%s owns the org of pe-a: got = (%v, %v), want (%v, nil)", name, got, err, owns)
		}
	}
	got, err := access.OwnsOrgOf(auth.WithPrincipal(context.Background(), accountCallers["owner"]), "pe-missing")
	if err != nil || got {
		t.Errorf("owner of a platform experiment with no row: got = (%v, %v), want (false, nil)", got, err)
	}
}
