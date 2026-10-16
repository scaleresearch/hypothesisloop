package db

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

func newTestUserHome(t *testing.T, accounts *AccountsStore) (userID, orgID string) {
	t.Helper()
	userID = "user-" + uuid.New().String()
	_, orgIDs, err := accounts.EnsureUserHome(context.Background(), userID, "name of "+userID, "home of "+userID)
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

	user, orgIDs, err := accounts.EnsureUserHome(ctx, userID, "name of "+userID, "another name")
	if err != nil {
		t.Fatalf("second request: got = %v, want = nil", err)
	}
	if user.Name != "name of "+userID || user.Staff || !slices.Equal(orgIDs, []string{orgID}) {
		t.Fatalf("second request: got staff=%v orgs=%v, want the same single org %s", user.Staff, orgIDs, orgID)
	}
	orgs, err := accounts.ListUserOrgs(ctx, userID)
	if err != nil || len(orgs) != 1 || orgs[0].Role != domain.OrgRoleOwner || orgs[0].Name != "home of "+userID {
		t.Fatalf("user orgs: got = %+v, %v, want one owned org keeping its first name", orgs, err)
	}
	defaultProjectOf(t, accounts, orgID)
}

func TestAUserCarriesTheNameLastStatedForThem(t *testing.T) {
	pool := eventsTestDB(t)
	accounts := NewAccountsStore(pool)
	ctx := context.Background()
	userID, orgID := newTestUserHome(t, accounts)

	for _, step := range []struct{ stated, want string }{
		{"Ada Lovelace", "Ada Lovelace"},
		{"", "Ada Lovelace"},
		{"Ada King", "Ada King"},
	} {
		user, _, err := accounts.EnsureUserHome(ctx, userID, step.stated, "ignored")
		if err != nil || user.Name != step.want {
			t.Fatalf("user after the provider states %q: got = %+v, %v, want name %q", step.stated, user, err, step.want)
		}
		var stored string
		if err := pool.pool.QueryRow(ctx, `SELECT name FROM users WHERE id = $1`, userID).Scan(&stored); err != nil || stored != step.want {
			t.Fatalf("stored name after the provider states %q: got = %q, %v, want = %q", step.stated, stored, err, step.want)
		}
	}

	members, _, err := accounts.ListOrgMembers(ctx, orgID, 20, 0)
	if err != nil || len(members) != 1 || members[0].Name != "Ada King" {
		t.Fatalf("members: got = %+v, %v, want the owner under their name", members, err)
	}
	key := &domain.APIKey{ID: "key-" + uuid.New().String(), OrgID: orgID, Actor: domain.Actor{Kind: domain.ActorUser, ID: userID}, Name: "laptop"}
	if err := accounts.CreateAPIKey(ctx, key, "hash-"+key.ID); err != nil || key.Actor.Name != "Ada King" {
		t.Fatalf("created key: got = %+v, %v, want its creator's name", key, err)
	}
	if read, err := accounts.GetAPIKey(ctx, key.ID); err != nil || read.Actor.Name != "Ada King" {
		t.Fatalf("read key: got = %+v, %v, want its creator's name", read, err)
	}
	invite := &domain.OrgInvite{ID: "inv-" + uuid.New().String(), OrgID: orgID, Role: domain.OrgRoleMember, Actor: domain.Actor{Kind: domain.ActorUser, ID: userID}, ExpiresAt: time.Now().Add(time.Hour)}
	if err := accounts.CreateOrgInvite(ctx, invite, "hash-"+invite.ID); err != nil || invite.Actor.Name != "Ada King" {
		t.Fatalf("created invite: got = %+v, %v, want its creator's name", invite, err)
	}
	if read, err := accounts.GetOrgInvite(ctx, invite.ID); err != nil || read.Actor != userActor(userID, "Ada King") || read.AcceptedActor != nil {
		t.Fatalf("read invite: got = %+v, %v, want its creator's name and nobody's as the one who accepted", read, err)
	}
	connectors := []domain.Actor{userActor(userID, "Ada King"), {Kind: domain.ActorKey, ID: key.ID, Name: "laptop"}, {Kind: domain.ActorStaff, Name: "staff"}}
	for n, want := range connectors {
		by := domain.Actor{Kind: want.Kind, ID: want.ID}
		installation := &domain.OrgGitHubInstallation{OrgID: orgID, InstallationID: int64(n + 1), AccountLogin: "acme-" + string(want.Kind), AccountType: "Organization", ConnectedActor: by}
		if err := accounts.ConnectGitHubInstallation(ctx, installation); err != nil || installation.ConnectedActor != want {
			t.Fatalf("installation connected by %+v: got = %+v, %v, want %+v", by, installation, err, want)
		}
		read, err := accounts.GitHubInstallationOfAccount(ctx, orgID, installation.AccountLogin)
		if err != nil || read == nil || read.ConnectedActor != want {
			t.Fatalf("read installation connected by %+v: got = %+v, %v, want %+v", by, read, err, want)
		}
	}

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
			_, results[i], errs[i] = accounts.EnsureUserHome(context.Background(), userID, "", "racing")
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

	second, err := accounts.CreateOrg(ctx, "second", userID, 0)
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

	project, err := accounts.CreateProject(ctx, second.ID, "vision", 0)
	if err != nil {
		t.Fatalf("create project: got = %v, want = nil", err)
	}
	if _, err := accounts.CreateProject(ctx, second.ID, "vision", 0); !errors.Is(err, ErrProjectNameTaken) {
		t.Fatalf("create a project under a taken name: got = %v, want = %v", err, ErrProjectNameTaken)
	}
	if _, err := accounts.CreateProject(ctx, homeOrg, "vision", 0); err != nil {
		t.Fatalf("the same project name in another org: got = %v, want = nil", err)
	}
	if _, err := accounts.CreateProject(ctx, "org_missing", "vision", 0); !errors.Is(err, ErrOrgNotFound) {
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
	addTestMember(t, accounts, orgID, owner, guest, domain.OrgRoleMember)
	if _, err := accounts.SetOrgMemberRole(ctx, orgID, guest, "admin"); err == nil {
		t.Fatal("a role nobody defined: got = nil, want an error")
	}
	if role, found, err := accounts.OrgRole(ctx, orgID, guest); err != nil || !found || role != domain.OrgRoleMember {
		t.Fatalf("role of the new member: got = %q, %v, %v, want member", role, found, err)
	}
	if role, _, _ := accounts.OrgRole(ctx, orgID, owner); role != domain.OrgRoleOwner {
		t.Fatalf("role of the creator: got = %q, want owner", role)
	}
	_, orgIDs, err := accounts.EnsureUserHome(ctx, guest, "", "ignored")
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

	orgKey := &domain.APIKey{ID: "k1" + suffix, OrgID: orgID, Actor: domain.Actor{Kind: domain.ActorUser, ID: userID}, Name: "ci"}
	if err := accounts.CreateAPIKey(ctx, orgKey, "hash-org-"+suffix); err != nil {
		t.Fatalf("create org key: got = %v, want = nil", err)
	}
	projectKey := &domain.APIKey{ID: "k2" + suffix, OrgID: orgID, ProjectID: project.ID, Actor: domain.Actor{Kind: domain.ActorUser, ID: userID}, Name: "one project"}
	if err := accounts.CreateAPIKey(ctx, projectKey, "hash-project-"+suffix); err != nil {
		t.Fatalf("create project key: got = %v, want = nil", err)
	}
	crossing := &domain.APIKey{ID: "k3" + suffix, OrgID: orgID, ProjectID: foreign.ID, Actor: domain.Actor{Kind: domain.ActorUser, ID: userID}, Name: "crossing"}
	if err := accounts.CreateAPIKey(ctx, crossing, "hash-crossing-"+suffix); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("key bound to another org's project: got = %v, want = %v", err, ErrProjectNotFound)
	}
	duplicate := &domain.APIKey{ID: "k4" + suffix, OrgID: orgID, Actor: domain.Actor{Kind: domain.ActorUser, ID: userID}, Name: "same hash"}
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
	createTestAgent(t, pool, agentID)
	seedExperimentForCapTest(t, pool, jobID, agentID, peID, 1, domain.StatusQueued)

	want := domain.Tenancy{PlatformExperimentID: peID, ProjectID: project.ID, OrgID: orgID, PriorRuns: []string{}}
	got, closedAt, err := accounts.PlatformExperimentTenancy(ctx, peID)
	if err != nil || got == nil || !reflect.DeepEqual(*got, want) || closedAt != nil {
		t.Fatalf("platform experiment tenancy: got = %+v closed at %v, %v, want %+v open", got, closedAt, err, want)
	}
	ofAgent := want
	ofAgent.AgentID, ofAgent.PriorRuns = agentID, nil
	job, ended, err := accounts.ExperimentTenancy(ctx, jobID)
	if err != nil || job == nil || !reflect.DeepEqual(*job, ofAgent) || ended {
		t.Fatalf("job tenancy: got = %+v ended=%v, %v, want %+v running", job, ended, err, ofAgent)
	}

	if _, err := pool.pool.Exec(ctx, `UPDATE experiments SET status = 'EVICTED', not_admitted_reason = NULL WHERE id = $1`, jobID); err != nil {
		t.Fatalf("end job: %v", err)
	}
	if _, ended, _ := accounts.ExperimentTenancy(ctx, jobID); !ended {
		t.Fatal("tenancy of an evicted job: got ended = false, want true")
	}
	pes := NewPlatformExperimentsStore(pool)
	if _, err := pool.pool.Exec(ctx, `UPDATE platform_experiments SET status = 'running' WHERE id = $1`, peID); err != nil {
		t.Fatalf("start platform experiment: %v", err)
	}
	if _, closedAt, _ := accounts.PlatformExperimentTenancy(ctx, peID); closedAt != nil {
		t.Fatalf("tenancy of a running platform experiment: got closed at %v, want nil", closedAt)
	}
	before := time.Now().Add(-time.Second)
	if err := pes.ClosePlatformExperiment(ctx, peID, nil, ""); err != nil {
		t.Fatalf("close platform experiment: %v", err)
	}
	_, closedAt, _ = accounts.PlatformExperimentTenancy(ctx, peID)
	if closedAt == nil || closedAt.Before(before) || closedAt.After(time.Now().Add(time.Second)) {
		t.Fatalf("tenancy of a closed platform experiment: got closed at %v, want about now", closedAt)
	}
	// The wind-down is counted from the close, which later writes to the row must not move.
	time.Sleep(20 * time.Millisecond)
	if err := pes.SetPlatformExperimentSummary(ctx, peID, "done"); err != nil {
		t.Fatalf("file summary: %v", err)
	}
	if err := pes.ClosePlatformExperiment(ctx, peID, &domain.Actor{Kind: domain.ActorStaff}, ""); err != nil {
		t.Fatalf("close again: %v", err)
	}
	if _, again, _ := accounts.PlatformExperimentTenancy(ctx, peID); again == nil || !again.Equal(*closedAt) {
		t.Fatalf("closed at after a summary and a second close: got = %v, want it unchanged from %v", again, closedAt)
	}

	if cutAt, err := pes.AgentCutAt(ctx, peID, agentID); err != nil || cutAt != nil {
		t.Fatalf("cut time of an agent that was not cut: got = %v, %v, want nil", cutAt, err)
	}
	if _, err := pool.pool.Exec(ctx, `INSERT INTO platform_experiment_cuts (platform_experiment_id, agent_id, stage_index) VALUES ($1, $2, 1)`, peID, agentID); err != nil {
		t.Fatalf("cut agent: %v", err)
	}
	if cutAt, err := pes.AgentCutAt(ctx, peID, agentID); err != nil || cutAt == nil || time.Since(*cutAt) > time.Minute {
		t.Fatalf("cut time of a cut agent: got = %v, %v, want about now", cutAt, err)
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

// The prior runs of a run are every other closed run of its project on its repository that
// ran, whatever it continues, latest close first: a sibling run nothing continued is read as its
// chain is, a run that only measured its baseline is one, and a draft closed before it ran is none.
func TestPriorRunsAreTheClosedRunsOfTheProjectOnTheSameRepository(t *testing.T) {
	pool := eventsTestDB(t)
	accounts := NewAccountsStore(pool)
	pes := NewPlatformExperimentsStore(pool)
	ctx := context.Background()
	_, orgID := newTestUserHome(t, accounts)
	project := defaultProjectOf(t, accounts, orgID)
	_, otherOrg := newTestUserHome(t, accounts)
	otherProject := defaultProjectOf(t, accounts, otherOrg)
	suffix := uuid.New().String()[:8]
	now := time.Now().UTC()
	seed := func(name, projectID, repository string, status domain.PlatformExperimentStatus, closedAgo time.Duration, continues string) string {
		id := "pe-prior-" + name + "-" + suffix
		createTestPEIn(t, pool, id, projectID)
		var closedAt *time.Time
		if status == domain.PlatformExpClosed {
			at := now.Add(-closedAgo)
			closedAt = &at
		}
		var startedAt *time.Time
		if status == domain.PlatformExpClosed && name != "never-ran" && name != "baseline-only" {
			at := now.Add(-closedAgo - time.Hour)
			startedAt = &at
		}
		baseline := ""
		if name == "baseline-only" {
			baseline = "job-baseline-" + suffix
		}
		if _, err := pool.pool.Exec(ctx, `UPDATE platform_experiments SET repository = $2, status = $3, closed_at = $4, continues = NULLIF($5, ''), started_at = $6, baseline_job_id = $7 WHERE id = $1`,
			id, repository, string(status), closedAt, continues, startedAt, baseline); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
		return id
	}
	first := seed("first", project.ID, "o/r", domain.PlatformExpClosed, 3*time.Hour, "")
	sibling := seed("sibling", project.ID, "o/r", domain.PlatformExpClosed, 2*time.Hour, "")
	continued := seed("continued", project.ID, "o/r", domain.PlatformExpClosed, time.Hour, first)
	seed("other-repository", project.ID, "o/x", domain.PlatformExpClosed, time.Hour, "")
	seed("still-running", project.ID, "o/r", domain.PlatformExpRunning, 0, "")
	seed("never-ran", project.ID, "o/r", domain.PlatformExpClosed, time.Hour, "")
	baselineOnly := seed("baseline-only", project.ID, "o/r", domain.PlatformExpClosed, 150*time.Minute, "")
	seed("other-project", otherProject.ID, "o/r", domain.PlatformExpClosed, time.Hour, "")
	run := seed("run", project.ID, "o/r", domain.PlatformExpRunning, 0, continued)
	unnamed := seed("unnamed", project.ID, "", domain.PlatformExpOpen, 0, "")

	for id, want := range map[string][]string{
		run:       {continued, sibling, baselineOnly, first},
		continued: {sibling, baselineOnly, first},
		unnamed:   {},
	} {
		got, err := pes.PriorRuns(ctx, id)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("prior runs of %s: got = %v, %v, want %v", id, got, err, want)
		}
		tenancy, _, err := accounts.PlatformExperimentTenancy(ctx, id)
		if err != nil || tenancy == nil || !reflect.DeepEqual(tenancy.PriorRuns, want) {
			t.Errorf("tenancy of %s: got = %+v, %v, want prior runs %v", id, tenancy, err, want)
		}
	}
}

// The standings of every prior run are read from one query of the relational store: each run in
// the order asked, with its completed jobs and its researchers.
func TestRunsToRankAreReadInTheOrderAsked(t *testing.T) {
	pool := eventsTestDB(t)
	pes := NewPlatformExperimentsStore(pool)
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	first, second := "pe-rank-first-"+suffix, "pe-rank-second-"+suffix
	researcher, baseline := "rank-researcher-"+suffix, "rank-baseline-"+suffix
	createTestPE(t, pool, first, 10)
	createTestPE(t, pool, second, 10)
	createTestAgent(t, pool, researcher)
	createTestAgent(t, pool, baseline)
	exec(t, pool, `INSERT INTO experiment_signups (platform_experiment_id, agent_id, quota_tier, added_by_kind, agent_type) VALUES ($1, $2, 'guaranteed', 'staff', 'generalist')`, first, researcher)
	exec(t, pool, `INSERT INTO experiment_signups (platform_experiment_id, agent_id, quota_tier, added_by_kind, agent_type, role) VALUES ($1, $2, 'guaranteed', 'staff', 'generalist', 'baseline')`, first, baseline)
	seedExperimentForCapTest(t, pool, "job-rank-done-"+suffix, researcher, first, 1, domain.StatusCompleted)
	seedExperimentForCapTest(t, pool, "job-rank-failed-"+suffix, researcher, first, 1, domain.StatusFailed)

	got, err := pes.RunsToRank(ctx, []string{second, "pe-rank-missing-" + suffix, first})
	if err != nil || len(got) != 2 || got[0].PE.ID != second || got[1].PE.ID != first {
		t.Fatalf("runs to rank: got = %+v, %v, want %s then %s", got, err, second, first)
	}
	if want := map[string]bool{"job-rank-done-" + suffix: true}; !reflect.DeepEqual(got[1].Completed, want) {
		t.Errorf("completed = %v, want %v", got[1].Completed, want)
	}
	if want := map[string]bool{researcher: true}; !reflect.DeepEqual(got[1].Competitors, want) {
		t.Errorf("competitors = %v, want %v", got[1].Competitors, want)
	}
	if len(got[0].Completed) != 0 || len(got[0].Competitors) != 0 {
		t.Errorf("a run with neither: got = %+v", got[0])
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
	secondA, err := accounts.CreateProject(ctx, orgA, "second", 0)
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
		if err := agents.CreateAgent(ctx, &domain.Agent{ID: id, Name: id, OrgID: org}); err != nil {
			t.Fatalf("create agent: %v", err)
		}
	}
	pes := NewPlatformExperimentsStore(pool)
	for pe, agent := range map[string]string{peA: agentA, peB: agentB} {
		if _, err := pes.Signup(ctx, pe, domain.AgentSignup{Actor: testActor, AgentID: agent, Role: domain.SignupRoleCompetitor}); err != nil {
			t.Fatalf("signup: %v", err)
		}
	}
	seedExperimentForCapTest(t, pool, "job-a-"+suffix, agentA, peA, 1, domain.StatusQueued)
	seedExperimentForCapTest(t, pool, "job-b-"+suffix, agentB, peB, 1, domain.StatusQueued)

	for _, tc := range []struct {
		name   string
		scope  domain.TenantScope
		pes    []string
		jobs   []string
		agents []string
	}{
		{"org A", domain.TenantScope{OrgIDs: []string{orgA}}, []string{peA, peA2}, []string{"job-a-" + suffix}, []string{agentA}},
		{"org B", domain.TenantScope{OrgIDs: []string{orgB}}, []string{peB}, []string{"job-b-" + suffix}, []string{agentB}},
		{"both orgs", domain.TenantScope{OrgIDs: []string{orgA, orgB}}, []string{peA, peA2, peB}, []string{"job-a-" + suffix, "job-b-" + suffix}, []string{agentA, agentB}},
		{"key bound to A's second project", domain.TenantScope{OrgIDs: []string{orgA}, ProjectID: secondA.ID}, []string{peA2}, nil, nil},
		{"A's key asking for B's project", domain.TenantScope{OrgIDs: []string{orgA}, ProjectID: projectB.ID}, nil, nil, nil},
		{"agent token of B's experiment", domain.TenantScope{OrgIDs: []string{orgB}, ProjectID: projectB.ID, PlatformExperimentID: peB}, []string{peB}, []string{"job-b-" + suffix}, []string{agentB}},
		{"no org", domain.TenantScope{}, nil, nil, nil},
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

// A job's project is its platform experiment's, wherever the job is read: one row or a list.
func TestAJobReadsItsProjectAndOrgOffItsPlatformExperiment(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	accounts := NewAccountsStore(pool)
	_, orgID := newTestUserHome(t, accounts)
	projectID := defaultProjectOf(t, accounts, orgID).ID
	suffix := uuid.New().String()
	agentID, peID := "agent-jp-"+suffix, "pe-jp-"+suffix
	if err := NewAgentsStore(pool).CreateAgent(ctx, &domain.Agent{ID: agentID, Name: agentID, OrgID: orgID}); err != nil {
		t.Fatalf("create agent: got = %v, want = nil", err)
	}
	createTestPEIn(t, pool, peID, projectID)

	parentID, childID := "job-jp-parent-"+suffix, "job-jp-child-"+suffix
	seedExperimentForCapTest(t, pool, parentID, agentID, peID, 1, domain.StatusQueued)
	seedExperimentForCapTest(t, pool, childID, agentID, peID, 1, domain.StatusQueued)
	exec(t, pool, `UPDATE experiments SET parent_id = $2 WHERE id = $1`, childID, parentID)

	store := NewExperimentsStore(pool)
	got, err := store.GetExperiment(ctx, childID)
	if err != nil || got.ProjectID != projectID || got.OrgID != orgID {
		t.Fatalf("get: got = %+v, %v, want project %q of org %q", got, err, projectID, orgID)
	}
	queued, err := store.ListQueuedExperiments(ctx)
	if err != nil {
		t.Fatalf("list queued: got = %v, want = nil", err)
	}
	for _, exp := range queued {
		if (exp.ID == parentID || exp.ID == childID) && exp.OrgID != orgID {
			t.Errorf("queued job %s: org = %q, want = %q", exp.ID, exp.OrgID, orgID)
		}
	}
	listed, err := store.ListExperiments(ctx, domain.ExperimentFilter{Scope: domain.TenantScope{AllOrgs: true, ProjectID: projectID}})
	if err != nil || len(listed) != 2 {
		t.Fatalf("list by project: got = %d jobs, %v, want = 2", len(listed), err)
	}
	for _, exp := range listed {
		if exp.ProjectID != projectID || exp.OrgID != orgID {
			t.Errorf("job %s: project = %q, org = %q, want = %q, %q", exp.ID, exp.ProjectID, exp.OrgID, projectID, orgID)
		}
	}
}
