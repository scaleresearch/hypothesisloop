package db

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

func newTestUserHome(t *testing.T, accounts *AccountsStore) (userID, orgID string) {
	t.Helper()
	userID = "user-" + uuid.New().String()
	_, orgIDs, err := accounts.EnsureUserHome(context.Background(), userID, "home of "+userID)
	if err != nil {
		t.Fatalf("ensure user home: got = %v, want = nil", err)
	}
	if len(orgIDs) != 1 {
		t.Fatalf("orgs of a new user: got = %v, want exactly one", orgIDs)
	}
	return userID, orgIDs[0]
}

func createTestPEIn(t *testing.T, pool *Pool, id, projectID string) {
	t.Helper()
	createTestPE(t, pool, id, 10)
	if _, err := pool.pool.Exec(context.Background(), `UPDATE platform_experiments SET project_id = $2 WHERE id = $1`, id, projectID); err != nil {
		t.Fatalf("place platform experiment: got = %v, want = nil", err)
	}
}

func defaultProjectOf(t *testing.T, accounts *AccountsStore, orgID string) *domain.Project {
	t.Helper()
	projects, err := accounts.ListProjects(context.Background(), []string{orgID})
	if err != nil || len(projects) != 1 || projects[0].Name != domain.DefaultProjectName {
		t.Fatalf("projects of a new org: got = %+v, %v, want one named %q", projects, err, domain.DefaultProjectName)
	}
	return projects[0]
}

func TestFirstRequestCreatesAUsersHomeOnce(t *testing.T) {
	pool := eventsTestDB(t)
	accounts := NewAccountsStore(pool)
	ctx := context.Background()
	userID, orgID := newTestUserHome(t, accounts)

	user, orgIDs, err := accounts.EnsureUserHome(ctx, userID, "another name")
	if err != nil {
		t.Fatalf("second request: got = %v, want = nil", err)
	}
	if user.Staff || !slices.Equal(orgIDs, []string{orgID}) {
		t.Fatalf("second request: got staff=%v orgs=%v, want the same single org %s", user.Staff, orgIDs, orgID)
	}
	orgs, err := accounts.ListUserOrgs(ctx, userID)
	if err != nil || len(orgs) != 1 || orgs[0].Role != domain.OrgRoleOwner || orgs[0].Name != "home of "+userID {
		t.Fatalf("user orgs: got = %+v, %v, want one owned org keeping its first name", orgs, err)
	}
	defaultProjectOf(t, accounts, orgID)
}

func TestConcurrentFirstRequestsEndWithOneOrg(t *testing.T) {
	pool := eventsTestDB(t)
	accounts := NewAccountsStore(pool)
	userID := "user-" + uuid.New().String()

	const callers = 8
	results := make([][]string, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, results[i], errs[i] = accounts.EnsureUserHome(context.Background(), userID, "racing")
		}()
	}
	wg.Wait()
	for i := range callers {
		if errs[i] != nil {
			t.Fatalf("caller %d: got = %v, want = nil", i, errs[i])
		}
		if len(results[i]) != 1 || results[i][0] != results[0][0] {
			t.Fatalf("caller %d saw orgs %v, caller 0 saw %v: want the same single org", i, results[i], results[0])
		}
	}
	var orgs, projects int
	if err := pool.pool.QueryRow(context.Background(), `
SELECT (SELECT COUNT(*) FROM org_members WHERE user_id = $1),
       (SELECT COUNT(*) FROM projects p JOIN org_members m ON m.org_id = p.org_id WHERE m.user_id = $1)`, userID).Scan(&orgs, &projects); err != nil {
		t.Fatalf("count: %v", err)
	}
	if orgs != 1 || projects != 1 {
		t.Fatalf("after %d concurrent first requests: got %d orgs and %d projects, want 1 and 1", callers, orgs, projects)
	}
}

func TestOrgsAndProjectsCanBeCreatedAndRenamed(t *testing.T) {
	pool := eventsTestDB(t)
	accounts := NewAccountsStore(pool)
	ctx := context.Background()
	userID, homeOrg := newTestUserHome(t, accounts)

	second, err := accounts.CreateOrg(ctx, "second", userID)
	if err != nil {
		t.Fatalf("create org: got = %v, want = nil", err)
	}
	defaultProjectOf(t, accounts, second.ID)
	if err := accounts.RenameOrg(ctx, second.ID, "renamed"); err != nil {
		t.Fatalf("rename org: got = %v, want = nil", err)
	}
	if err := accounts.RenameOrg(ctx, "org_missing", "x"); !errors.Is(err, ErrOrgNotFound) {
		t.Fatalf("rename a missing org: got = %v, want = %v", err, ErrOrgNotFound)
	}
	orgs, err := accounts.ListUserOrgs(ctx, userID)
	if err != nil || len(orgs) != 2 || orgs[0].ID != homeOrg || orgs[1].Name != "renamed" {
		t.Fatalf("user orgs: got = %+v, %v, want the home org then the renamed one", orgs, err)
	}

	project, err := accounts.CreateProject(ctx, second.ID, "vision")
	if err != nil {
		t.Fatalf("create project: got = %v, want = nil", err)
	}
	if _, err := accounts.CreateProject(ctx, second.ID, "vision"); !errors.Is(err, ErrProjectNameTaken) {
		t.Fatalf("create a project under a taken name: got = %v, want = %v", err, ErrProjectNameTaken)
	}
	if _, err := accounts.CreateProject(ctx, homeOrg, "vision"); err != nil {
		t.Fatalf("the same project name in another org: got = %v, want = nil", err)
	}
	if _, err := accounts.CreateProject(ctx, "org_missing", "vision"); !errors.Is(err, ErrOrgNotFound) {
		t.Fatalf("create a project in a missing org: got = %v, want = %v", err, ErrOrgNotFound)
	}
	if err := accounts.RenameProject(ctx, project.ID, domain.DefaultProjectName); !errors.Is(err, ErrProjectNameTaken) {
		t.Fatalf("rename onto a taken name: got = %v, want = %v", err, ErrProjectNameTaken)
	}
	if err := accounts.RenameProject(ctx, project.ID, "language"); err != nil {
		t.Fatalf("rename project: got = %v, want = nil", err)
	}
	if err := accounts.RenameProject(ctx, "prj_missing", "x"); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("rename a missing project: got = %v, want = %v", err, ErrProjectNotFound)
	}
	got, err := accounts.GetProject(ctx, project.ID)
	if err != nil || got == nil || got.Name != "language" || got.OrgID != second.ID {
		t.Fatalf("get project: got = %+v, %v, want it renamed in org %s", got, err, second.ID)
	}
	if missing, err := accounts.GetProject(ctx, "prj_missing"); err != nil || missing != nil {
		t.Fatalf("get a missing project: got = %+v, %v, want nil, nil", missing, err)
	}
}

func TestMembershipIsPerOrg(t *testing.T) {
	pool := eventsTestDB(t)
	accounts := NewAccountsStore(pool)
	ctx := context.Background()
	owner, orgID := newTestUserHome(t, accounts)
	guest, guestOrg := newTestUserHome(t, accounts)

	if role, found, err := accounts.OrgRole(ctx, orgID, guest); err != nil || found {
		t.Fatalf("role of a non-member: got = %q, %v, %v, want not found", role, found, err)
	}
	if err := accounts.AddOrgMember(ctx, orgID, guest, domain.OrgRoleMember); err != nil {
		t.Fatalf("add member: got = %v, want = nil", err)
	}
	if err := accounts.AddOrgMember(ctx, orgID, guest, "admin"); err == nil {
		t.Fatal("add member with an unknown role: got = nil, want an error")
	}
	if role, found, err := accounts.OrgRole(ctx, orgID, guest); err != nil || !found || role != domain.OrgRoleMember {
		t.Fatalf("role of the new member: got = %q, %v, %v, want member", role, found, err)
	}
	if role, _, _ := accounts.OrgRole(ctx, orgID, owner); role != domain.OrgRoleOwner {
		t.Fatalf("role of the creator: got = %q, want owner", role)
	}
	_, orgIDs, err := accounts.EnsureUserHome(ctx, guest, "ignored")
	if err != nil || !slices.Equal(orgIDs, []string{guestOrg, orgID}) {
		t.Fatalf("orgs of a user in two: got = %v, %v, want home first then the joined one", orgIDs, err)
	}
}

func TestAPIKeyIsFoundByHashUntilRevoked(t *testing.T) {
	pool := eventsTestDB(t)
	accounts := NewAccountsStore(pool)
	ctx := context.Background()
	userID, orgID := newTestUserHome(t, accounts)
	_, otherOrg := newTestUserHome(t, accounts)
	project := defaultProjectOf(t, accounts, orgID)
	foreign := defaultProjectOf(t, accounts, otherOrg)
	suffix := uuid.New().String()[:8]

	orgKey := &domain.APIKey{ID: "k1" + suffix, OrgID: orgID, CreatedBy: userID, Name: "ci"}
	if err := accounts.CreateAPIKey(ctx, orgKey, "hash-org-"+suffix); err != nil {
		t.Fatalf("create org key: got = %v, want = nil", err)
	}
	projectKey := &domain.APIKey{ID: "k2" + suffix, OrgID: orgID, ProjectID: project.ID, CreatedBy: userID, Name: "one project"}
	if err := accounts.CreateAPIKey(ctx, projectKey, "hash-project-"+suffix); err != nil {
		t.Fatalf("create project key: got = %v, want = nil", err)
	}
	crossing := &domain.APIKey{ID: "k3" + suffix, OrgID: orgID, ProjectID: foreign.ID, CreatedBy: userID, Name: "crossing"}
	if err := accounts.CreateAPIKey(ctx, crossing, "hash-crossing-"+suffix); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("key bound to another org's project: got = %v, want = %v", err, ErrProjectNotFound)
	}
	duplicate := &domain.APIKey{ID: "k4" + suffix, OrgID: orgID, CreatedBy: userID, Name: "same hash"}
	if err := accounts.CreateAPIKey(ctx, duplicate, "hash-org-"+suffix); err == nil {
		t.Fatal("a second key under the same hash: got = nil, want an error")
	}

	got, err := accounts.APIKeyByHash(ctx, "hash-project-"+suffix)
	if err != nil || got == nil || got.ID != projectKey.ID || got.OrgID != orgID || got.ProjectID != project.ID || got.RevokedAt != nil {
		t.Fatalf("lookup by hash: got = %+v, %v, want the live project key", got, err)
	}
	if got, err := accounts.APIKeyByHash(ctx, "hash-unknown-"+suffix); err != nil || got != nil {
		t.Fatalf("lookup of an unknown hash: got = %+v, %v, want nil, nil", got, err)
	}

	if err := accounts.RevokeAPIKey(ctx, orgKey.ID); err != nil {
		t.Fatalf("revoke: got = %v, want = nil", err)
	}
	revoked, err := accounts.APIKeyByHash(ctx, "hash-org-"+suffix)
	if err != nil || revoked == nil || revoked.RevokedAt == nil {
		t.Fatalf("lookup of a revoked key: got = %+v, %v, want it with revoked_at set", revoked, err)
	}
	if err := accounts.RevokeAPIKey(ctx, orgKey.ID); err != nil {
		t.Fatalf("revoke again: got = %v, want = nil", err)
	}
	again, _ := accounts.GetAPIKey(ctx, orgKey.ID)
	if again == nil || !again.RevokedAt.Equal(*revoked.RevokedAt) {
		t.Fatalf("revoked_at after a second revoke: got = %+v, want it unchanged from %v", again, revoked.RevokedAt)
	}
	if err := accounts.RevokeAPIKey(ctx, "missing"+suffix); !errors.Is(err, ErrAPIKeyNotFound) {
		t.Fatalf("revoke a missing key: got = %v, want = %v", err, ErrAPIKeyNotFound)
	}

	keys, err := accounts.ListAPIKeys(ctx, orgID)
	if err != nil || len(keys) != 2 {
		t.Fatalf("list keys: got = %+v, %v, want the two created, revoked one included", keys, err)
	}
	if keys, err := accounts.ListAPIKeys(ctx, otherOrg); err != nil || len(keys) != 0 {
		t.Fatalf("list keys of another org: got = %+v, %v, want none", keys, err)
	}
}

func TestTenancyIsDerivedThroughTheProject(t *testing.T) {
	pool := eventsTestDB(t)
	accounts := NewAccountsStore(pool)
	ctx := context.Background()
	_, orgID := newTestUserHome(t, accounts)
	project := defaultProjectOf(t, accounts, orgID)
	suffix := uuid.New().String()[:8]
	peID, agentID, jobID := "pe-ten-"+suffix, "agent-ten-"+suffix, "job-ten-"+suffix
	createTestPEIn(t, pool, peID, project.ID)
	createTestAgent(t, pool, agentID, domain.AgentKindAgent)
	seedExperimentForCapTest(t, pool, jobID, agentID, peID, 1, domain.StatusQueued)

	want := domain.Tenancy{PlatformExperimentID: peID, ProjectID: project.ID, OrgID: orgID}
	got, closed, err := accounts.PlatformExperimentTenancy(ctx, peID)
	if err != nil || got == nil || *got != want || closed {
		t.Fatalf("platform experiment tenancy: got = %+v closed=%v, %v, want %+v open", got, closed, err, want)
	}
	job, ended, err := accounts.ExperimentTenancy(ctx, jobID)
	if err != nil || job == nil || *job != want || ended {
		t.Fatalf("job tenancy: got = %+v ended=%v, %v, want %+v running", job, ended, err, want)
	}

	if _, err := pool.pool.Exec(ctx, `UPDATE experiments SET status = 'EVICTED', not_admitted_reason = NULL WHERE id = $1`, jobID); err != nil {
		t.Fatalf("end job: %v", err)
	}
	if _, ended, _ := accounts.ExperimentTenancy(ctx, jobID); !ended {
		t.Fatal("tenancy of an evicted job: got ended = false, want true")
	}
	if _, err := pool.pool.Exec(ctx, `UPDATE platform_experiments SET status = 'closed' WHERE id = $1`, peID); err != nil {
		t.Fatalf("close platform experiment: %v", err)
	}
	if _, closed, _ := accounts.PlatformExperimentTenancy(ctx, peID); !closed {
		t.Fatal("tenancy of a closed platform experiment: got closed = false, want true")
	}

	if got, _, err := accounts.PlatformExperimentTenancy(ctx, "pe-missing-"+suffix); err != nil || got != nil {
		t.Fatalf("tenancy of a missing platform experiment: got = %+v, %v, want nil, nil", got, err)
	}
	if got, _, err := accounts.ExperimentTenancy(ctx, "job-missing-"+suffix); err != nil || got != nil {
		t.Fatalf("tenancy of a missing job: got = %+v, %v, want nil, nil", got, err)
	}
	if org, found, err := accounts.AgentOrg(ctx, agentID); err != nil || !found || org != "default" {
		t.Fatalf("agent org: got = %q, %v, %v, want default", org, found, err)
	}
	if _, found, err := accounts.AgentOrg(ctx, "agent-missing-"+suffix); err != nil || found {
		t.Fatalf("org of a missing agent: got found = %v, %v, want false", found, err)
	}
}

// Every list read takes the caller's scope. This holds two orgs side by side and reads each
// listing as each of them, as a key bound to one project, and as an agent token.
func TestListingsKeepToTheCallersScope(t *testing.T) {
	pool := eventsTestDB(t)
	accounts := NewAccountsStore(pool)
	ctx := context.Background()
	_, orgA := newTestUserHome(t, accounts)
	_, orgB := newTestUserHome(t, accounts)
	projectA, projectB := defaultProjectOf(t, accounts, orgA), defaultProjectOf(t, accounts, orgB)
	secondA, err := accounts.CreateProject(ctx, orgA, "second")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	suffix := uuid.New().String()[:8]
	peA, peA2, peB := "pe-a-"+suffix, "pe-a2-"+suffix, "pe-b-"+suffix
	createTestPEIn(t, pool, peA, projectA.ID)
	createTestPEIn(t, pool, peA2, secondA.ID)
	createTestPEIn(t, pool, peB, projectB.ID)

	agents := NewAgentsStore(pool)
	agentA, agentB := "agent-a-"+suffix, "agent-b-"+suffix
	for id, org := range map[string]string{agentA: orgA, agentB: orgB} {
		if err := agents.CreateAgent(ctx, &domain.Agent{ID: id, Name: id, OrgID: org, Kind: domain.AgentKindAgent}); err != nil {
			t.Fatalf("create agent: %v", err)
		}
	}
	pes := NewPlatformExperimentsStore(pool)
	for pe, agent := range map[string]string{peA: agentA, peB: agentB} {
		if _, err := pes.Signup(ctx, pe, agent, domain.SignupRoleCompetitor, ""); err != nil {
			t.Fatalf("signup: %v", err)
		}
	}
	seedExperimentForCapTest(t, pool, "job-a-"+suffix, agentA, peA, 1, domain.StatusQueued)
	seedExperimentForCapTest(t, pool, "job-b-"+suffix, agentB, peB, 1, domain.StatusQueued)
	donations := NewDonationStore(pool)
	for pe, agent := range map[string]string{peA: agentA, peB: agentB} {
		if err := donations.CreateDonationRequest(ctx, &domain.DonationRequest{
			ID: uuid.New().String(), AgentID: agent, PlatformExperimentID: pe, CreditsWant: 1, Reason: suffix, Status: "open",
		}); err != nil {
			t.Fatalf("create donation: %v", err)
		}
	}

	for _, tc := range []struct {
		name      string
		scope     domain.TenantScope
		pes       []string
		jobs      []string
		agents    []string
		donations int
	}{
		{"org A", domain.TenantScope{OrgIDs: []string{orgA}}, []string{peA, peA2}, []string{"job-a-" + suffix}, []string{agentA}, 1},
		{"org B", domain.TenantScope{OrgIDs: []string{orgB}}, []string{peB}, []string{"job-b-" + suffix}, []string{agentB}, 1},
		{"both orgs", domain.TenantScope{OrgIDs: []string{orgA, orgB}}, []string{peA, peA2, peB}, []string{"job-a-" + suffix, "job-b-" + suffix}, []string{agentA, agentB}, 2},
		{"key bound to A's second project", domain.TenantScope{OrgIDs: []string{orgA}, ProjectID: secondA.ID}, []string{peA2}, nil, nil, 0},
		{"A's key asking for B's project", domain.TenantScope{OrgIDs: []string{orgA}, ProjectID: projectB.ID}, nil, nil, nil, 0},
		{"agent token of B's experiment", domain.TenantScope{OrgIDs: []string{orgB}, ProjectID: projectB.ID, PlatformExperimentID: peB}, []string{peB}, []string{"job-b-" + suffix}, []string{agentB}, 1},
		{"no org", domain.TenantScope{}, nil, nil, nil, 0},
	} {
		got, err := pes.ListPlatformExperiments(ctx, PlatformExperimentsFilter{Scope: tc.scope, Limit: 200})
		if err != nil {
			t.Fatalf("%s: list platform experiments: %v", tc.name, err)
		}
		var ids []string
		for _, pe := range got {
			ids = append(ids, pe.ID)
		}
		assertSameSet(t, tc.name+": platform experiments", ids, tc.pes)
		if n, err := pes.CountPlatformExperiments(ctx, PlatformExperimentsFilter{Scope: tc.scope}); err != nil || n != len(tc.pes) {
			t.Errorf("%s: count platform experiments: got = %d, %v, want = %d", tc.name, n, err, len(tc.pes))
		}
		visible, err := accounts.PlatformExperimentIDs(ctx, tc.scope)
		if err != nil {
			t.Fatalf("%s: platform experiment ids: %v", tc.name, err)
		}
		assertSameSet(t, tc.name+": platform experiment ids", visible, tc.pes)

		experiments := NewExperimentsStore(pool)
		jobs, err := experiments.ListExperiments(ctx, domain.ExperimentFilter{Scope: tc.scope})
		if err != nil {
			t.Fatalf("%s: list jobs: %v", tc.name, err)
		}
		ids = nil
		for _, job := range jobs {
			ids = append(ids, job.ID)
		}
		assertSameSet(t, tc.name+": jobs", ids, tc.jobs)
		if n, err := experiments.CountExperiments(ctx, domain.ExperimentFilter{Scope: tc.scope}); err != nil || n != len(tc.jobs) {
			t.Errorf("%s: count jobs: got = %d, %v, want = %d", tc.name, n, err, len(tc.jobs))
		}
		stats, err := experiments.ExperimentStats(ctx, domain.ExperimentFilter{Scope: tc.scope}, "")
		if err != nil || stats.Total != len(tc.jobs) {
			t.Errorf("%s: job stats: got = %+v, %v, want total %d", tc.name, stats, err, len(tc.jobs))
		}

		listed, err := agents.ListAgents(ctx, AgentsFilter{Scope: tc.scope, Limit: 200})
		if err != nil {
			t.Fatalf("%s: list agents: %v", tc.name, err)
		}
		ids = nil
		for _, agent := range listed {
			ids = append(ids, agent.ID)
		}
		assertSameSet(t, tc.name+": agents", ids, tc.agents)
		if stats, err := agents.AgentStats(ctx, AgentsFilter{Scope: tc.scope}); err != nil || stats.Total != len(tc.agents) {
			t.Errorf("%s: agent stats: got = %+v, %v, want total %d", tc.name, stats, err, len(tc.agents))
		}

		requests, err := donations.ListDonationRequests(ctx, tc.scope, "", "", 200, 0)
		if err != nil || len(requests) != tc.donations {
			t.Errorf("%s: donations: got = %d, %v, want = %d", tc.name, len(requests), err, tc.donations)
		}
		if n, err := donations.CountDonationRequests(ctx, tc.scope, "", ""); err != nil || n != tc.donations {
			t.Errorf("%s: count donations: got = %d, %v, want = %d", tc.name, n, err, tc.donations)
		}
	}
}

func assertSameSet(t *testing.T, what string, got, want []string) {
	t.Helper()
	got, want = slices.Clone(got), slices.Clone(want)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("%s: got = %v, want = %v", what, got, want)
	}
}
