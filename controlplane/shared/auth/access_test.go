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

func by(t domain.Tenancy, agentID string) domain.Tenancy {
	t.AgentID = agentID
	return t
}

func twoOrgs() *auth.Access {
	return auth.NewAccess(authtest.Placement{
		PlatformExperiments: map[string]domain.Tenancy{"pe-a": inA, "pe-a2": inA2, "pe-b": inB},
		Experiments:         map[string]domain.Tenancy{"job-a": by(inA, "agent-a"), "job-a-other": by(inA, "agent-a-other"), "job-a2": inA2, "job-b": inB},
		Agents:              map[string]string{"agent-a": "org-a", "agent-a-other": "org-a", "agent-b": "org-b"},
		Clusters:            map[string]string{"cluster-a": "org-a", "cluster-b": "org-b"},
		Projects: map[string]domain.Project{
			"prj-a": {ID: "prj-a", OrgID: "org-a"}, "prj-a2": {ID: "prj-a2", OrgID: "org-a"}, "prj-b": {ID: "prj-b", OrgID: "org-b"},
		},
	})
}

var callers = map[string]auth.Principal{
	"user of A":                     {Kind: auth.KindUser, UserID: "u", OrgIDs: []string{"org-a"}},
	"user of A and B":               {Kind: auth.KindUser, UserID: "u2", OrgIDs: []string{"org-a", "org-b"}},
	"key of A":                      {Kind: auth.KindKey, OrgIDs: []string{"org-a"}},
	"key of A's project a":          {Kind: auth.KindKey, OrgIDs: []string{"org-a"}, ProjectID: "prj-a"},
	"agent in pe-a":                 {Kind: auth.KindAgent, AgentID: "agent-a", PlatformExperimentID: "pe-a", ProjectID: "prj-a", OrgIDs: []string{"org-a"}},
	"agent token for an agent of B": {Kind: auth.KindAgent, AgentID: "agent-b", PlatformExperimentID: "pe-a", ProjectID: "prj-a", OrgIDs: []string{"org-a"}},
	"agent token for no agent":      {Kind: auth.KindAgent, AgentID: "agent-unregistered", PlatformExperimentID: "pe-a", ProjectID: "prj-a", OrgIDs: []string{"org-a"}},
	"coordinator of pe-a":           {Kind: auth.KindCoordinator, PlatformExperimentID: "pe-a", ProjectID: "prj-a", OrgIDs: []string{"org-a"}},
	"job job-a":                     {Kind: auth.KindJob, ExperimentID: "job-a", PlatformExperimentID: "pe-a", ProjectID: "prj-a", OrgIDs: []string{"org-a"}},
	"staff":                         {Kind: auth.KindStaff},
	"user with no org":              {Kind: auth.KindUser, UserID: "u3"},
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
		{"user of A", "pe-missing", 404}, {"agent in pe-a", "pe-missing", 404}, {"staff", "pe-missing", 200},
		{"user of A", "pe-b/agent-1", 404}, {"user of A and B", "pe-b/agent-1", 404}, {"staff", "pe-b/agent-1", 404},
		{"user of A", "pe-*", 404}, {"user of A", "", 404}, {"staff", "", 404},
	})
}

func TestAnAbsentRowAndAnotherOrgsRowReadTheSame(t *testing.T) {
	access := twoOrgs()
	ctx := auth.WithPrincipal(context.Background(), callers["user of A"])
	for what, pair := range map[string][2]error{
		"platform experiment": {access.PlatformExperiment(ctx, "pe-b"), access.PlatformExperiment(ctx, "pe-missing")},
		"hypothesis":          {access.Within(ctx, "hypothesis", "pe-b"), access.Within(ctx, "hypothesis", "pe-missing")},
		"job":                 {access.Experiment(ctx, "job-b"), access.Experiment(ctx, "job-missing")},
		"agent":               {access.Agent(ctx, "agent-b"), access.Agent(ctx, "agent-unregistered")},
		"project":             {access.Project(ctx, "prj-b"), access.Project(ctx, "prj-missing")},
	} {
		theirs, absent := pair[0], pair[1]
		if status(theirs) != 404 || status(absent) != 404 || theirs.Error() != absent.Error() {
			t.Errorf("%s: another org's answers %q, an absent one %q, want the same 404", what, theirs, absent)
		}
	}
}

func TestExperimentAccess(t *testing.T) {
	runChecks(t, "job", twoOrgs().Experiment, []check{
		{"user of A", "job-a", 200}, {"user of A", "job-b", 404},
		{"key of A's project a", "job-a2", 404},
		{"agent in pe-a", "job-a", 200}, {"agent in pe-a", "job-a2", 404}, {"agent in pe-a", "job-b", 404},
		{"coordinator of pe-a", "job-a2", 404},
		{"job job-a", "job-a", 200}, {"job job-a", "job-a2", 403}, {"job job-a", "job-b", 403}, {"job job-a", "job-missing", 403},
		{"staff", "job-b", 200},
		{"user of A", "job-missing", 404}, {"agent in pe-a", "job-missing", 404}, {"staff", "job-missing", 200},
		{"user of A", "job-b/x", 404}, {"staff", "job-b/x", 404},
		{"agent in pe-a", "job-a-other", 200},
	})
}

func TestAnAgentActsOnItsOwnJobsOnly(t *testing.T) {
	runChecks(t, "own job", twoOrgs().OwnExperiment, []check{
		{"agent in pe-a", "job-a", 200}, {"agent in pe-a", "job-a-other", 403},
		{"agent in pe-a", "job-b", 404}, {"agent in pe-a", "job-missing", 404},
		{"user of A", "job-a-other", 200}, {"coordinator of pe-a", "job-a-other", 200}, {"key of A", "job-a-other", 200},
		{"job job-a", "job-a", 200}, {"job job-a", "job-a-other", 403},
		{"staff", "job-a-other", 200}, {"staff", "job-missing", 200},
	})
}

func TestNamingAnAgent(t *testing.T) {
	access := twoOrgs()
	runChecks(t, "agent", access.Agent, []check{
		{"agent in pe-a", "agent-a", 200}, {"agent in pe-a", "agent-a-other", 403}, {"agent in pe-a", "agent-b", 403},
		{"user of A", "agent-a", 200}, {"user of A", "agent-a-other", 200}, {"user of A", "agent-b", 404},
		{"key of A", "agent-b", 404},
		{"coordinator of pe-a", "agent-a-other", 200}, {"coordinator of pe-a", "agent-b", 404},
		{"staff", "agent-b", 200}, {"staff", "agent-unregistered", 200},
		{"user of A", "agent-unregistered", 404},
		{"agent token for an agent of B", "agent-b", 404},
		{"agent token for no agent", "agent-unregistered", 404},
		{"user of A", "agent-*", 404}, {"staff", "agent-*", 404},
	})
	runChecks(t, "self", access.Self, []check{
		{"agent in pe-a", "agent-a", 200}, {"agent in pe-a", "agent-new", 403},
		{"user of A", "agent-b", 200},
	})
	// A job's agent and platform experiment: an agent's token says both, so it may leave them out
	// and may not state another. Anyone else states both.
	for _, c := range []struct {
		caller, pe, agent string
		wantPE, wantAgent string
		want              int
	}{
		{"agent in pe-a", "", "", "pe-a", "agent-a", 200},
		{"agent in pe-a", "pe-a", "agent-a", "pe-a", "agent-a", 200},
		{"agent in pe-a", "pe-a2", "", "", "", 404},
		{"agent in pe-a", "", "agent-a-other", "", "", 403},
		{"user of A", "pe-a", "agent-a-other", "pe-a", "agent-a-other", 200},
		{"user of A", "", "agent-a", "", "", 404},
		{"coordinator of pe-a", "pe-a", "agent-a", "", "", 403},
	} {
		ctx := auth.WithPrincipal(context.Background(), callers[c.caller])
		pe, agent, err := access.Spender(ctx, c.pe, c.agent)
		if status(err) != c.want || pe != c.wantPE || agent != c.wantAgent {
			t.Errorf("spender: %s stating (%q, %q): got = (%q, %q, %d), want (%q, %q, %d)", c.caller, c.pe, c.agent, pe, agent, status(err), c.wantPE, c.wantAgent, c.want)
		}
	}
}

func TestOnlyStaffPassTheStaffCheck(t *testing.T) {
	access := twoOrgs()
	for name, caller := range callers {
		want := 403
		if caller.Kind == auth.KindStaff {
			want = 200
		}
		if got := status(access.Staff(auth.WithPrincipal(context.Background(), caller))); got != want {
			t.Errorf("%s: got = %d, want = %d", name, got, want)
		}
	}
	if got := status(access.Staff(context.Background())); got != 401 {
		t.Errorf("no principal: got = %d, want = 401", got)
	}
}

func TestProjectAccess(t *testing.T) {
	runChecks(t, "project", twoOrgs().Project, []check{
		{"user of A", "prj-a", 200}, {"user of A", "prj-b", 404}, {"user of A", "prj-missing", 404},
		{"key of A's project a", "prj-a", 200}, {"key of A's project a", "prj-a2", 404},
		{"coordinator of pe-a", "prj-a", 403}, {"coordinator of pe-a", "prj-a2", 403},
		{"agent in pe-a", "prj-a", 403}, {"job job-a", "prj-a", 403},
		{"key of A", "prj-a", 200}, {"key of A", "prj-a2", 200}, {"key of A", "prj-b", 404},
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
		"agent":               access.Agent(ctx, "agent-a"),
		"project":             access.Project(ctx, "prj-a"),
	} {
		if status(err) != http.StatusUnauthorized {
			t.Errorf("%s with no principal: got = %d, want = 401", name, status(err))
		}
	}
}

// The owner of a record is the agent a request names, and the actor is the caller whatever the
// request names.
func TestWhoseRecordAWriteIsAndWhoseCredentialMadeIt(t *testing.T) {
	access := twoOrgs()
	ada := auth.Principal{Kind: auth.KindUser, UserID: "user-ada", Name: "Ada Lovelace", OrgIDs: []string{"org-a"}}
	nameless := auth.Principal{Kind: auth.KindUser, UserID: "user-x", OrgIDs: []string{"org-a"}}
	staffUser := auth.Principal{Kind: auth.KindStaff, UserID: "user-ops", Name: "Ops"}
	adaActor := domain.Actor{Kind: domain.ActorUser, ID: "user-ada", Name: "Ada Lovelace"}
	agent := domain.Actor{Kind: domain.ActorAgent, ID: callers["agent in pe-a"].AgentID}
	key := domain.Actor{Kind: domain.ActorKey, ID: callers["key of A"].KeyID}
	for name, tc := range map[string]struct {
		caller  auth.Principal
		agentID string
		owner   string
		actor   domain.Actor
		status  int
	}{
		"agent as itself":                         {callers["agent in pe-a"], "agent-a", "agent-a", agent, 200},
		"agent naming no agent":                   {callers["agent in pe-a"], "", "agent-a", agent, 200},
		"agent as another agent":                  {callers["agent in pe-a"], "agent-a-other", "", domain.Actor{}, 403},
		"user as themselves":                      {ada, "", "", adaActor, 200},
		"user for an agent of their org":          {ada, "agent-a", "agent-a", adaActor, 200},
		"user for an agent of another org":        {ada, "agent-b", "", domain.Actor{}, 404},
		"user the provider gave no name":          {nameless, "", "", domain.Actor{Kind: domain.ActorUser, ID: "user-x"}, 200},
		"staff user as themselves":                {staffUser, "", "", domain.Actor{Kind: domain.ActorUser, ID: "user-ops", Name: "Ops"}, 200},
		"staff user for any agent":                {staffUser, "agent-b", "agent-b", domain.Actor{Kind: domain.ActorUser, ID: "user-ops", Name: "Ops"}, 200},
		"staff token":                             {auth.Principal{Kind: auth.KindStaff}, "", "", domain.Actor{Kind: domain.ActorStaff}, 200},
		"key as itself":                           {callers["key of A"], "", "", key, 200},
		"key for an agent":                        {callers["key of A"], "agent-a", "agent-a", key, 200},
		"key for an agent of another org":         {callers["key of A"], "agent-b", "", domain.Actor{}, 404},
		"coordinator as itself":                   {callers["coordinator of pe-a"], "", "", domain.Actor{Kind: domain.ActorCoordinator, ID: callers["coordinator of pe-a"].PlatformExperimentID}, 200},
		"coordinator for an agent of another org": {callers["coordinator of pe-a"], "agent-b", "", domain.Actor{}, 404},
		"runtime":  {auth.Principal{Kind: auth.KindRuntime, ClusterName: "c", OrgIDs: []string{"org-a"}}, "", "", domain.Actor{}, 403},
		"launcher": {auth.Principal{Kind: auth.KindLauncher, LauncherName: "l"}, "", "", domain.Actor{}, 403},
	} {
		owner, actor, err := access.Writer(auth.WithPrincipal(context.Background(), tc.caller), tc.agentID)
		if status(err) != tc.status || owner != tc.owner || actor != tc.actor {
			t.Errorf("%s: got = owner %q, actor %+v, %d (%v), want = owner %q, actor %+v, %d", name, owner, actor, status(err), err, tc.owner, tc.actor, tc.status)
		}
	}
	if _, _, err := access.Writer(context.Background(), ""); status(err) != 401 {
		t.Errorf("no caller: got = %d (%v), want = 401", status(err), err)
	}
}

// An agent or a coordinator reads each prior run of its own platform experiment, and what is
// placed in it, as its own, and writes nothing to it. A job reads no prior run, and a run of the
// project that is no prior run stays out of reach.
func TestAnAgentOrACoordinatorReadsThePriorRunsOfItsOwn(t *testing.T) {
	prior := domain.Tenancy{PlatformExperimentID: "pe-prior", ProjectID: "prj-a", OrgID: "org-a"}
	other := domain.Tenancy{PlatformExperimentID: "pe-other", ProjectID: "prj-a", OrgID: "org-a"}
	access := auth.NewAccess(authtest.Placement{
		PlatformExperiments: map[string]domain.Tenancy{"pe-a": inA, "pe-prior": prior, "pe-other": other},
		Experiments:         map[string]domain.Tenancy{"job-prior": by(prior, "agent-prior")},
		Projects:            map[string]domain.Project{"prj-a": {ID: "prj-a", OrgID: "org-a"}},
	})
	for _, tc := range []struct {
		caller          string
		reads, readsJob int
	}{
		{"agent in pe-a", 200, 200}, {"coordinator of pe-a", 200, 200}, {"job job-a", 404, 403},
	} {
		caller, reads := tc.caller, tc.reads
		p := callers[caller]
		p.PriorRuns = []string{"pe-prior"}
		write := auth.WithPrincipal(context.Background(), p)
		read := auth.WithRead(write, "pe-prior")
		for _, c := range []struct {
			what string
			err  error
			want int
		}{
			{"the prior run", access.PlatformExperiment(read, "pe-prior"), reads},
			{"its hypotheses", access.Within(read, "hypothesis", "pe-prior"), reads},
			{"a job of it", access.Experiment(read, "job-prior"), tc.readsJob},
			{"the prior run, to write", access.PlatformExperiment(write, "pe-prior"), 404},
			{"a run that is no prior run", access.PlatformExperiment(read, "pe-other"), 404},
		} {
			if got := status(c.err); got != c.want {
				t.Errorf("%s, %s: got = %d, want = %d", caller, c.what, got, c.want)
			}
		}
		if scope, err := access.ListScope(read, ""); reads == 200 && (err != nil || scope.PlatformExperimentID != "pe-prior") {
			t.Errorf("%s listing the prior run: got scope %+v, %v, want it narrowed to pe-prior", caller, scope, err)
		}
	}
}
