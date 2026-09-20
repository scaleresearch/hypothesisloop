package accounts

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

const testSigningSecret = "accounts-test-signing-secret"

var testRuntimeAddresses = RuntimeAddresses{APIURL: "https://api.test.invalid", MetricsURL: "https://metrics.test.invalid"}

// world is two orgs on one API. org A has an owner, a member, a second project, three API keys
// and a cluster; org B has its own owner, key and cluster. Each caller is a token named after
// what it is.
type world struct {
	t       *testing.T
	store   *memStore
	router  http.Handler
	callers map[string]auth.Principal

	orgA, orgB           string
	projectA, projectA2  string
	projectB             string
	clusterA, clusterB   string
	personKey, parentKey string
}

// tokens resolves a token to the principal of that name, and refuses a revoked API key as the
// authenticator does.
type tokens struct{ w *world }

func (a tokens) Authenticate(_ context.Context, token string) (auth.Principal, error) {
	p, ok := a.w.callers[token]
	if !ok {
		return auth.Principal{}, auth.Unauthenticated("unknown token")
	}
	if p.Kind == auth.KindKey && a.w.store.asRead(a.w.store.keys[p.KeyID]).RevokedAt != nil {
		return auth.Principal{}, auth.Unauthenticated("api key " + p.KeyID + " is revoked")
	}
	return p, nil
}

func newWorld(t *testing.T) *world {
	t.Helper()
	ctx := context.Background()
	w := &world{t: t, store: newMemStore(), clusterA: "cluster-a", clusterB: "cluster-b"}
	for _, user := range []string{"u-owner", "u-member", "u-other", "u-staff"} {
		w.store.users[user] = true
	}
	orgA, _ := w.store.CreateOrg(ctx, "org a", "u-owner")
	orgB, _ := w.store.CreateOrg(ctx, "org b", "u-other")
	w.orgA, w.orgB = orgA.ID, orgB.ID
	w.store.roles[w.orgA]["u-member"] = domain.OrgRoleMember
	projectsA, _ := w.store.ListProjects(ctx, []string{w.orgA})
	projectsB, _ := w.store.ListProjects(ctx, []string{w.orgB})
	w.projectA, w.projectB = projectsA[0].ID, projectsB[0].ID
	second, _ := w.store.CreateProject(ctx, w.orgA, "second")
	w.projectA2 = second.ID
	w.personKey, w.parentKey = "k-person", "k-org"
	for _, key := range []*domain.APIKey{
		{ID: "k-person", OrgID: w.orgA, CreatedBy: "u-owner", Name: "made by the owner"},
		{ID: "k-org", OrgID: w.orgA, CreatedBy: "u-owner", Name: "org-wide"},
		{ID: "k-prj", OrgID: w.orgA, ProjectID: w.projectA, CreatedBy: "u-owner", Name: "one project"},
		{ID: "k-foreign", OrgID: w.orgB, CreatedBy: "u-other", Name: "of org b"},
	} {
		if err := w.store.CreateAPIKey(ctx, key, "hash-"+key.ID); err != nil {
			t.Fatalf("seed key %s: %v", key.ID, err)
		}
	}
	if _, err := w.store.RegisterCluster(ctx, w.clusterA, w.orgA); err != nil {
		t.Fatalf("seed cluster: %v", err)
	}
	if _, err := w.store.RegisterCluster(ctx, w.clusterB, w.orgB); err != nil {
		t.Fatalf("seed cluster: %v", err)
	}
	inA := func(kind auth.Kind) auth.Principal {
		return auth.Principal{Kind: kind, OrgIDs: []string{w.orgA}, ProjectID: w.projectA, PlatformExperimentID: "pe-a"}
	}
	w.callers = map[string]auth.Principal{
		"owner":       {Kind: auth.KindUser, UserID: "u-owner", Name: "Owner", OrgIDs: []string{w.orgA}},
		"member":      {Kind: auth.KindUser, UserID: "u-member", Name: "Member", OrgIDs: []string{w.orgA}},
		"outsider":    {Kind: auth.KindUser, UserID: "u-other", Name: "Other", OrgIDs: []string{w.orgB}},
		"org-key":     {Kind: auth.KindKey, KeyID: "k-org", OrgIDs: []string{w.orgA}},
		"project-key": {Kind: auth.KindKey, KeyID: "k-prj", OrgIDs: []string{w.orgA}, ProjectID: w.projectA},
		"foreign-key": {Kind: auth.KindKey, KeyID: "k-foreign", OrgIDs: []string{w.orgB}},
		"agent":       func() auth.Principal { p := inA(auth.KindAgent); p.AgentID = "agent-1"; return p }(),
		"coordinator": inA(auth.KindCoordinator),
		"job":         func() auth.Principal { p := inA(auth.KindJob); p.ExperimentID = "job-1"; return p }(),
		"runtime":     {Kind: auth.KindRuntime, ClusterName: w.clusterA, OrgIDs: []string{w.orgA}},
		"launcher":    {Kind: auth.KindLauncher, LauncherName: "launcher-1"},
		"staff":       {Kind: auth.KindStaff},
		"staff-user":  {Kind: auth.KindStaff, UserID: "u-staff", Name: "Staff", OrgIDs: []string{}},
	}
	router := chi.NewRouter()
	doc := apidocs.New(router, "test", "1.0.0", "", tokens{w}, auth.NewAccess(authtest.Placement{}), zap.NewNop())
	connected := func(context.Context) (map[string]bool, error) { return map[string]bool{w.clusterA: true}, nil }
	RegisterHuma(doc, NewHandler(w.store, w.store, connected, testSigningSecret, testRuntimeAddresses))
	w.router = router
	return w
}

// call makes one request as caller; an empty caller sends no token.
func (w *world) call(caller, method, path string, body any) *httptest.ResponseRecorder {
	w.t.Helper()
	var payload bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&payload).Encode(body); err != nil {
			w.t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &payload)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if caller != "" {
		req.Header.Set("Authorization", "Bearer "+caller)
	}
	rec := httptest.NewRecorder()
	w.router.ServeHTTP(rec, req)
	return rec
}

// ok makes a request that must answer want and decodes its body into out.
func (w *world) ok(caller, method, path string, body any, want int, out any) {
	w.t.Helper()
	rec := w.call(caller, method, path, body)
	if rec.Code != want {
		w.t.Fatalf("%s %s %s: got = %d %s, want = %d", caller, method, path, rec.Code, rec.Body, want)
	}
	if out != nil {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			w.t.Fatalf("%s %s %s: decode %s: %v", caller, method, path, rec.Body, err)
		}
	}
}

type name map[string]any

// Every operation against org A's resources, by every kind of caller. The gate in front of the
// handlers refuses the tokens the platform derives for agents, jobs, runtimes and launchers; the
// policy refuses the rest.
func TestWhoMayCallWhat(t *testing.T) {
	const (
		ok        = http.StatusOK
		created   = http.StatusCreated
		bad       = http.StatusBadRequest
		noToken   = http.StatusUnauthorized
		forbidden = http.StatusForbidden
		notFound  = http.StatusNotFound
	)
	type request struct {
		method, path string
		body         any
	}
	derived := map[string]int{"agent": forbidden, "coordinator": forbidden, "job": forbidden, "runtime": forbidden, "launcher": forbidden, "": noToken}
	for _, op := range []struct {
		name    string
		request func(w *world) request
		want    map[string]int
	}{
		{"read me", func(w *world) request { return request{"GET", "/me", nil} },
			map[string]int{"owner": ok, "member": ok, "outsider": ok, "org-key": ok, "project-key": ok, "foreign-key": ok, "staff": ok, "staff-user": ok}},
		{"create an org", func(w *world) request { return request{"POST", "/orgs", name{"name": "new"}} },
			map[string]int{"owner": created, "member": created, "outsider": created, "org-key": forbidden, "project-key": forbidden, "foreign-key": forbidden, "staff": bad, "staff-user": created}},
		{"rename an org", func(w *world) request { return request{"PATCH", "/orgs/" + w.orgA, name{"name": "renamed"}} },
			map[string]int{"owner": ok, "member": forbidden, "outsider": notFound, "org-key": ok, "project-key": forbidden, "foreign-key": notFound, "staff": ok, "staff-user": ok}},
		{"list projects", func(w *world) request { return request{"GET", "/orgs/" + w.orgA + "/projects", nil} },
			map[string]int{"owner": ok, "member": ok, "outsider": notFound, "org-key": ok, "project-key": ok, "foreign-key": notFound, "staff": ok, "staff-user": ok}},
		{"create a project", func(w *world) request { return request{"POST", "/orgs/" + w.orgA + "/projects", name{"name": "third"}} },
			map[string]int{"owner": created, "member": forbidden, "outsider": notFound, "org-key": created, "project-key": forbidden, "foreign-key": notFound, "staff": created, "staff-user": created}},
		{"rename a project", func(w *world) request { return request{"PATCH", "/projects/" + w.projectA2, name{"name": "renamed"}} },
			map[string]int{"owner": ok, "member": forbidden, "outsider": notFound, "org-key": ok, "project-key": forbidden, "foreign-key": notFound, "staff": ok, "staff-user": ok}},
		{"create an org-wide key", func(w *world) request { return request{"POST", "/orgs/" + w.orgA + "/api-keys", name{"name": "new"}} },
			map[string]int{"owner": created, "member": forbidden, "outsider": notFound, "org-key": created, "project-key": forbidden, "foreign-key": notFound, "staff": forbidden, "staff-user": created}},
		{"create a key of one project", func(w *world) request {
			return request{"POST", "/orgs/" + w.orgA + "/api-keys", name{"name": "new", "project_id": w.projectA}}
		},
			map[string]int{"owner": created, "member": forbidden, "outsider": notFound, "org-key": created, "project-key": created, "foreign-key": notFound, "staff": forbidden, "staff-user": created}},
		{"create a key of another project", func(w *world) request {
			return request{"POST", "/orgs/" + w.orgA + "/api-keys", name{"name": "new", "project_id": w.projectA2}}
		},
			map[string]int{"owner": created, "member": forbidden, "outsider": notFound, "org-key": created, "project-key": forbidden, "foreign-key": notFound, "staff": forbidden, "staff-user": created}},
		{"list keys", func(w *world) request { return request{"GET", "/orgs/" + w.orgA + "/api-keys", nil} },
			map[string]int{"owner": ok, "member": forbidden, "outsider": notFound, "org-key": ok, "project-key": ok, "foreign-key": notFound, "staff": ok, "staff-user": ok}},
		{"revoke a key a person made", func(w *world) request { return request{"DELETE", "/api-keys/" + w.personKey, nil} },
			map[string]int{"owner": ok, "member": forbidden, "outsider": notFound, "org-key": notFound, "project-key": notFound, "foreign-key": notFound, "staff": ok, "staff-user": ok}},
		{"register a cluster", func(w *world) request {
			return request{"POST", "/orgs/" + w.orgA + "/clusters", name{"name": "new-cluster"}}
		},
			map[string]int{"owner": ok, "member": forbidden, "outsider": notFound, "org-key": ok, "project-key": forbidden, "foreign-key": notFound, "staff": ok, "staff-user": ok}},
		{"list clusters", func(w *world) request { return request{"GET", "/orgs/" + w.orgA + "/clusters", nil} },
			map[string]int{"owner": ok, "member": ok, "outsider": notFound, "org-key": ok, "project-key": forbidden, "foreign-key": notFound, "staff": ok, "staff-user": ok}},
		{"share a cluster", func(w *world) request {
			return request{"PATCH", "/orgs/" + w.orgA + "/clusters/" + w.clusterA, map[string]any{"shared": true}}
		},
			map[string]int{"owner": forbidden, "member": forbidden, "outsider": forbidden, "org-key": forbidden, "project-key": forbidden, "foreign-key": forbidden, "staff": ok, "staff-user": ok}},
	} {
		for caller, want := range derived {
			op.want[caller] = want
		}
		for caller, want := range op.want {
			label := caller
			if label == "" {
				label = "no token"
			}
			t.Run(op.name+"/"+label, func(t *testing.T) {
				w := newWorld(t)
				if _, known := w.callers[caller]; !known && caller != "" {
					t.Fatalf("no caller named %q", caller)
				}
				before := snapshot(t, w.store)
				r := op.request(w)
				rec := w.call(caller, r.method, r.path, r.body)
				if rec.Code != want {
					t.Fatalf("%s %s: got = %d %s, want = %d", r.method, r.path, rec.Code, rec.Body, want)
				}
				if want >= 300 && snapshot(t, w.store) != before {
					t.Fatalf("%s %s was refused with %d and still changed the store", r.method, r.path, want)
				}
			})
		}
		if len(op.want) != len(newWorld(t).callers)+1 {
			t.Errorf("%s: %d callers expected, %d exist", op.name, len(op.want), len(newWorld(t).callers)+1)
		}
	}
}

// snapshot is everything the store holds, so a refused request can be shown to have written nothing.
func snapshot(t *testing.T, s *memStore) string {
	t.Helper()
	raw, err := json.Marshal([]any{s.orgs, s.roles, s.projects, s.keys, s.hashes, s.clusters})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	return string(raw)
}

func TestMeShowsWhatTheCallerReaches(t *testing.T) {
	w := newWorld(t)
	ids := func(me Me) (orgs, projects []string) {
		for _, org := range me.Orgs {
			orgs = append(orgs, org.ID)
		}
		for _, project := range me.Projects {
			projects = append(projects, project.ID)
		}
		return orgs, projects
	}
	for caller, want := range map[string]struct {
		kind     auth.Kind
		orgs     []string
		projects []string
		role     domain.OrgRole
	}{
		"owner":       {auth.KindUser, []string{w.orgA}, []string{w.projectA, w.projectA2}, domain.OrgRoleOwner},
		"member":      {auth.KindUser, []string{w.orgA}, []string{w.projectA, w.projectA2}, domain.OrgRoleMember},
		"outsider":    {auth.KindUser, []string{w.orgB}, []string{w.projectB}, domain.OrgRoleOwner},
		"org-key":     {auth.KindKey, []string{w.orgA}, []string{w.projectA, w.projectA2}, ""},
		"project-key": {auth.KindKey, []string{w.orgA}, []string{w.projectA}, ""},
		"staff":       {auth.KindStaff, []string{w.orgA, w.orgB}, []string{w.projectA, w.projectB, w.projectA2}, ""},
	} {
		var me Me
		w.ok(caller, "GET", "/me", nil, http.StatusOK, &me)
		orgs, projects := ids(me)
		if me.Kind != want.kind || strings.Join(orgs, ",") != strings.Join(want.orgs, ",") || strings.Join(projects, ",") != strings.Join(want.projects, ",") {
			t.Errorf("%s: got = %s orgs %v projects %v, want = %s orgs %v projects %v", caller, me.Kind, orgs, projects, want.kind, want.orgs, want.projects)
		}
		if me.Orgs[0].Role != want.role {
			t.Errorf("%s: role = %q, want = %q", caller, me.Orgs[0].Role, want.role)
		}
	}
	var key, me Me
	w.ok("project-key", "GET", "/me", nil, http.StatusOK, &key)
	if key.KeyID != "k-prj" || key.ProjectID != w.projectA || key.UserID != "" {
		t.Errorf("a project key's me: got = %+v, want key k-prj bound to %s", key, w.projectA)
	}
	w.ok("owner", "GET", "/me", nil, http.StatusOK, &me)
	if me.UserID != "u-owner" || me.Name != "Owner" || me.KeyID != "" {
		t.Errorf("a person's me: got = %+v, want user u-owner named Owner", me)
	}
}

func TestAnOrgIsCreatedForItsOwner(t *testing.T) {
	w := newWorld(t)
	var org domain.Org
	w.ok("member", "POST", "/orgs", name{"name": "  second home  "}, http.StatusCreated, &org)
	if org.Name != "second home" || org.Role != domain.OrgRoleOwner {
		t.Fatalf("created org: got = %+v, want it named second home and owned", org)
	}
	if role := w.store.roles[org.ID]["u-member"]; role != domain.OrgRoleOwner {
		t.Fatalf("role of the creator: got = %q, want owner", role)
	}
	if projects, _ := w.store.ListProjects(context.Background(), []string{org.ID}); len(projects) != 1 || projects[0].Name != domain.DefaultProjectName {
		t.Fatalf("projects of a new org: got = %+v, want one named default", projects)
	}

	if rec := w.call("member", "POST", "/orgs", name{"name": "for another", "owner_user_id": "u-owner"}); rec.Code != http.StatusForbidden {
		t.Fatalf("a person creating an org for another: got = %d %s, want = 403", rec.Code, rec.Body)
	}
	w.ok("staff", "POST", "/orgs", name{"name": "by staff", "owner_user_id": "u-other"}, http.StatusCreated, &org)
	if role := w.store.roles[org.ID]["u-other"]; role != domain.OrgRoleOwner {
		t.Fatalf("owner of an org staff created: got = %q, want u-other as owner", role)
	}
	if rec := w.call("staff", "POST", "/orgs", name{"name": "x", "owner_user_id": "u-nobody"}); rec.Code != http.StatusNotFound {
		t.Fatalf("an org for a user that does not exist: got = %d %s, want = 404", rec.Code, rec.Body)
	}
	for _, invalid := range []string{"", "   ", strings.Repeat("n", maxNameLength+1)} {
		if rec := w.call("owner", "POST", "/orgs", name{"name": invalid}); rec.Code != http.StatusBadRequest {
			t.Fatalf("org named %q: got = %d %s, want = 400", invalid, rec.Code, rec.Body)
		}
	}
}

func TestRenamesKeepIdsAndRefuseATakenProjectName(t *testing.T) {
	w := newWorld(t)
	var org domain.Org
	w.ok("owner", "PATCH", "/orgs/"+w.orgA, name{"name": "org b"}, http.StatusOK, &org)
	if org.ID != w.orgA || org.Name != "org b" {
		t.Fatalf("renamed org: got = %+v, want %s named like another org", org, w.orgA)
	}
	if rec := w.call("staff", "PATCH", "/orgs/org-missing", name{"name": "x"}); rec.Code != http.StatusNotFound {
		t.Fatalf("rename an org that does not exist: got = %d %s, want = 404", rec.Code, rec.Body)
	}

	var project domain.Project
	w.ok("owner", "PATCH", "/projects/"+w.projectA2, name{"name": "experiments"}, http.StatusOK, &project)
	if project.ID != w.projectA2 || project.Name != "experiments" || project.OrgID != w.orgA {
		t.Fatalf("renamed project: got = %+v, want %s named experiments", project, w.projectA2)
	}
	w.ok("owner", "PATCH", "/projects/"+w.projectA2, name{"name": "experiments"}, http.StatusOK, nil)
	for _, r := range []struct{ method, path string }{
		{"PATCH", "/projects/" + w.projectA2},
		{"POST", "/orgs/" + w.orgA + "/projects"},
	} {
		if rec := w.call("owner", r.method, r.path, name{"name": domain.DefaultProjectName}); rec.Code != http.StatusConflict {
			t.Fatalf("%s %s to a name the org has: got = %d %s, want = 409", r.method, r.path, rec.Code, rec.Body)
		}
	}
	w.ok("outsider", "POST", "/orgs/"+w.orgB+"/projects", name{"name": "experiments"}, http.StatusCreated, nil)
	if rec := w.call("owner", "PATCH", "/projects/prj-missing", name{"name": "x"}); rec.Code != http.StatusNotFound {
		t.Fatalf("rename a project that does not exist: got = %d %s, want = 404", rec.Code, rec.Body)
	}
	missing := w.call("owner", "PATCH", "/projects/prj-missing", name{"name": "x"}).Body.String()
	foreign := w.call("owner", "PATCH", "/projects/"+w.projectB, name{"name": "x"}).Body.String()
	if missing != foreign {
		t.Fatalf("another org's project answers %s, a missing one %s: the two must not differ", foreign, missing)
	}
}

func TestAProjectKeySeesItsProjectOnly(t *testing.T) {
	w := newWorld(t)
	var listed struct {
		Projects []domain.Project `json:"projects"`
	}
	w.ok("project-key", "GET", "/orgs/"+w.orgA+"/projects", nil, http.StatusOK, &listed)
	if len(listed.Projects) != 1 || listed.Projects[0].ID != w.projectA {
		t.Fatalf("projects a project key lists: got = %+v, want only %s", listed.Projects, w.projectA)
	}
	w.ok("org-key", "GET", "/orgs/"+w.orgA+"/projects", nil, http.StatusOK, &listed)
	if len(listed.Projects) != 2 {
		t.Fatalf("projects an org key lists: got = %+v, want both", listed.Projects)
	}
}

func TestAKeysSecretIsReturnedOnceAndNeverListed(t *testing.T) {
	w := newWorld(t)
	var created CreatedAPIKey
	rec := w.call("owner", "POST", "/orgs/"+w.orgA+"/api-keys", name{"name": "ci", "project_id": w.projectA2})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: got = %d %s, want = 201", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(created.Token, "hlk_"+created.ID+"_") || created.CreatedBy != "u-owner" || created.ProjectID != w.projectA2 || created.MintedBy != "" {
		t.Fatalf("created key: got = %+v, want an hlk_ token of u-owner bound to %s", created, w.projectA2)
	}
	if w.store.hashes[created.ID] != auth.HashAPIKey(created.Token) {
		t.Fatal("the key is not stored under the hash of the token that was returned")
	}
	secret := strings.TrimPrefix(created.Token, "hlk_"+created.ID+"_")

	for _, read := range []struct{ caller, method, path string }{
		{"owner", "GET", "/orgs/" + w.orgA + "/api-keys"},
		{"staff", "GET", "/orgs/" + w.orgA + "/api-keys"},
		{"owner", "GET", "/me"},
		{"owner", "DELETE", "/api-keys/" + created.ID},
		{"owner", "DELETE", "/api-keys/" + created.ID},
	} {
		rec := w.call(read.caller, read.method, read.path, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s: got = %d %s, want = 200", read.method, read.path, rec.Code, rec.Body)
		}
		for _, leak := range []string{secret, created.Token, w.store.hashes[created.ID], "token", "key_hash"} {
			if strings.Contains(rec.Body.String(), leak) {
				t.Fatalf("%s %s shows %q: %s", read.method, read.path, leak, rec.Body)
			}
		}
	}
	if rec := w.call("owner", "POST", "/orgs/"+w.orgA+"/api-keys", name{"name": "x", "project_id": w.projectB}); rec.Code != http.StatusNotFound {
		t.Fatalf("a key bound to another org's project: got = %d %s, want = 404", rec.Code, rec.Body)
	}
	if rec := w.call("owner", "POST", "/orgs/"+w.orgA+"/api-keys", name{"name": ""}); rec.Code != http.StatusBadRequest {
		t.Fatalf("a key with no name: got = %d %s, want = 400", rec.Code, rec.Body)
	}
}

// mint creates a key as caller and makes its token a caller of the same name as the key.
func (w *world) mint(caller, keyName, projectID string) CreatedAPIKey {
	w.t.Helper()
	var created CreatedAPIKey
	body := name{"name": keyName}
	if projectID != "" {
		body["project_id"] = projectID
	}
	w.ok(caller, "POST", "/orgs/"+w.orgA+"/api-keys", body, http.StatusCreated, &created)
	w.callers[keyName] = auth.Principal{Kind: auth.KindKey, KeyID: created.ID, OrgIDs: []string{w.orgA}, ProjectID: projectID}
	return created
}

func (w *world) listedKeys(caller string) []string {
	w.t.Helper()
	var listed struct {
		APIKeys []domain.APIKey `json:"api_keys"`
	}
	w.ok(caller, "GET", "/orgs/"+w.orgA+"/api-keys", nil, http.StatusOK, &listed)
	ids := []string{}
	for _, k := range listed.APIKeys {
		ids = append(ids, k.ID)
	}
	return ids
}

func TestAKeyReachesOnlyTheKeysMintedFromIt(t *testing.T) {
	w := newWorld(t)
	child := w.mint("org-key", "child", "")
	grandchild := w.mint("child", "grandchild", w.projectA2)
	if child.MintedBy != "k-org" || grandchild.MintedBy != child.ID {
		t.Fatalf("minted_by: got = %q, %q, want k-org and %s", child.MintedBy, grandchild.MintedBy, child.ID)
	}
	if child.CreatedBy != "u-owner" || grandchild.CreatedBy != "u-owner" {
		t.Fatalf("created_by of minted keys: got = %q, %q, want the person behind the first key", child.CreatedBy, grandchild.CreatedBy)
	}

	for caller, want := range map[string][]string{
		"org-key":     {grandchild.ID, child.ID, "k-org"},
		"child":       {grandchild.ID, child.ID},
		"grandchild":  {grandchild.ID},
		"project-key": {"k-prj"},
		"owner":       {grandchild.ID, child.ID, "k-prj", "k-org", "k-person"},
	} {
		if got := w.listedKeys(caller); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("keys %s lists: got = %v, want = %v", caller, got, want)
		}
	}

	for _, refused := range []struct{ caller, target string }{
		{"child", "k-org"}, {"child", "k-person"}, {"child", "k-prj"},
		{"grandchild", child.ID}, {"project-key", "k-org"}, {"org-key", "k-prj"}, {"org-key", "k-foreign"},
	} {
		if rec := w.call(refused.caller, "DELETE", "/api-keys/"+refused.target, nil); rec.Code != http.StatusNotFound {
			t.Errorf("%s revoking %s: got = %d %s, want = 404", refused.caller, refused.target, rec.Code, rec.Body)
		}
	}
	if rec := w.call("owner", "DELETE", "/api-keys/k-foreign", nil); rec.Code != http.StatusNotFound {
		t.Errorf("an owner revoking another org's key: got = %d %s, want = 404", rec.Code, rec.Body)
	}
	for id, key := range w.store.keys {
		if key.RevokedAt != nil {
			t.Fatalf("key %s was revoked by a refused request", id)
		}
	}
}

func TestAKeyCannotMintAWiderKey(t *testing.T) {
	w := newWorld(t)
	for _, wider := range []name{{"name": "org-wide"}, {"name": "other project", "project_id": w.projectA2}} {
		if rec := w.call("project-key", "POST", "/orgs/"+w.orgA+"/api-keys", wider); rec.Code != http.StatusForbidden {
			t.Fatalf("a project key minting %v: got = %d %s, want = 403", wider, rec.Code, rec.Body)
		}
	}
	same := w.mint("project-key", "same-project", w.projectA)
	if same.ProjectID != w.projectA || same.MintedBy != "k-prj" {
		t.Fatalf("a project key minting for its project: got = %+v, want it bound to %s", same, w.projectA)
	}
	if rec := w.call("org-key", "POST", "/orgs/"+w.orgB+"/api-keys", name{"name": "x"}); rec.Code != http.StatusNotFound {
		t.Fatalf("a key minting in another org: got = %d %s, want = 404", rec.Code, rec.Body)
	}
	if rec := w.call("org-key", "POST", "/orgs/"+w.orgA+"/api-keys", name{"name": "x", "project_id": w.projectB}); rec.Code != http.StatusNotFound {
		t.Fatalf("a key minting for another org's project: got = %d %s, want = 404", rec.Code, rec.Body)
	}
}

func TestRevokingAKeyEndsItAndEveryKeyMintedFromIt(t *testing.T) {
	w := newWorld(t)
	child := w.mint("org-key", "child", "")
	grandchild := w.mint("child", "grandchild", w.projectA)
	sibling := w.mint("org-key", "sibling", "")

	var revoked domain.APIKey
	w.ok("child", "DELETE", "/api-keys/"+child.ID, nil, http.StatusOK, &revoked)
	if revoked.ID != child.ID || revoked.RevokedAt == nil {
		t.Fatalf("a key revoking itself: got = %+v, want it revoked", revoked)
	}
	for _, ended := range []string{"child", "grandchild"} {
		for _, r := range []struct{ method, path string }{{"GET", "/me"}, {"GET", "/orgs/" + w.orgA + "/api-keys"}, {"POST", "/orgs/" + w.orgA + "/api-keys"}} {
			if rec := w.call(ended, r.method, r.path, name{"name": "after"}); rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s after revocation, %s %s: got = %d %s, want = 401", ended, r.method, r.path, rec.Code, rec.Body)
			}
		}
	}
	for _, alive := range []string{"org-key", "sibling"} {
		w.ok(alive, "GET", "/me", nil, http.StatusOK, nil)
	}
	read := func(id string) *domain.APIKey { return w.store.asRead(w.store.keys[id]) }
	if read(grandchild.ID).RevokedAt == nil || read(sibling.ID).RevokedAt != nil || read("k-org").RevokedAt != nil {
		t.Fatal("revocation did not follow the lineage exactly: descendants end, the parent and siblings stay")
	}

	first := *w.store.keys[child.ID].RevokedAt
	w.ok("owner", "DELETE", "/api-keys/"+child.ID, nil, http.StatusOK, &revoked)
	if !revoked.RevokedAt.Equal(first) {
		t.Fatalf("revoked_at after a second revoke: got = %v, want it unchanged from %v", revoked.RevokedAt, first)
	}
	if rec := w.call("owner", "DELETE", "/api-keys/k-missing", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("revoke a key that does not exist: got = %d %s, want = 404", rec.Code, rec.Body)
	}
}

func TestRegisteringAClusterReturnsItsRuntimeToken(t *testing.T) {
	w := newWorld(t)
	want, err := auth.Mint(testSigningSecret, auth.RuntimeScope("gpu-east"))
	if err != nil {
		t.Fatal(err)
	}
	for _, caller := range []string{"owner", "org-key", "owner"} {
		var registered RegisteredCluster
		w.ok(caller, "POST", "/orgs/"+w.orgA+"/clusters", name{"name": "gpu-east"}, http.StatusOK, &registered)
		if registered.RuntimeToken != want || registered.OrgID != w.orgA || registered.Shared {
			t.Fatalf("registered by %s: got = %+v, want the derived token, owned by %s and not shared", caller, registered, w.orgA)
		}
		if registered.APIURL != testRuntimeAddresses.APIURL || registered.MetricsURL != testRuntimeAddresses.MetricsURL {
			t.Fatalf("registered by %s: got = %s and %s, want the addresses runtimes are configured to use", caller, registered.APIURL, registered.MetricsURL)
		}
	}

	rec := w.call("outsider", "POST", "/orgs/"+w.orgB+"/clusters", name{"name": "gpu-east"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("a name another org owns: got = %d %s, want = 409", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), want) || strings.Contains(rec.Body.String(), "runtime_token") {
		t.Fatalf("the refusal carries the cluster's token: %s", rec.Body)
	}
	if w.store.clusters["gpu-east"].OrgID != w.orgA {
		t.Fatal("the cluster changed hands")
	}
	for _, invalid := range []string{"", "Not A Name", "has/slash"} {
		if rec := w.call("owner", "POST", "/orgs/"+w.orgA+"/clusters", name{"name": invalid}); rec.Code != http.StatusBadRequest {
			t.Fatalf("cluster named %q: got = %d %s, want = 400", invalid, rec.Code, rec.Body)
		}
	}
	if rec := w.call("staff", "POST", "/orgs/org-missing/clusters", name{"name": "gpu-west"}); rec.Code != http.StatusNotFound {
		t.Fatalf("a cluster for an org that does not exist: got = %d %s, want = 404", rec.Code, rec.Body)
	}
}

func TestClustersAreListedForTheirOrgWithConnectivity(t *testing.T) {
	w := newWorld(t)
	w.ok("owner", "POST", "/orgs/"+w.orgA+"/clusters", name{"name": "idle"}, http.StatusOK, nil)
	var listed struct {
		Clusters []ListedCluster `json:"clusters"`
	}
	rec := w.call("member", "GET", "/orgs/"+w.orgA+"/clusters", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: got = %d %s, want = 200", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Clusters) != 2 || listed.Clusters[0].Name != w.clusterA || !listed.Clusters[0].Connected || listed.Clusters[1].Name != "idle" || listed.Clusters[1].Connected {
		t.Fatalf("clusters of org a: got = %+v, want cluster-a connected and idle not", listed.Clusters)
	}
	if strings.Contains(rec.Body.String(), "runtime_token") || strings.Contains(rec.Body.String(), "hls_") {
		t.Fatalf("the listing carries a runtime token: %s", rec.Body)
	}
}

func TestASharedClusterIsListedToEveryOrgWithoutItsOwnerOrItsSettingsKey(t *testing.T) {
	w := newWorld(t)
	w.store.clusters[w.clusterA].ReportedID = "id-of-a"
	w.store.clusters[w.clusterB].ReportedID = "id-of-b"
	list := func(caller, orgID string) (map[string]ListedCluster, string) {
		t.Helper()
		rec := w.call(caller, "GET", "/orgs/"+orgID+"/clusters", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s lists clusters of %s: got = %d %s, want = 200", caller, orgID, rec.Code, rec.Body)
		}
		var listed struct {
			Clusters []ListedCluster `json:"clusters"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
			t.Fatal(err)
		}
		byName := map[string]ListedCluster{}
		for _, c := range listed.Clusters {
			byName[c.Name] = c
		}
		return byName, rec.Body.String()
	}

	for _, caller := range []string{"outsider", "foreign-key"} {
		if listed, _ := list(caller, w.orgB); len(listed) != 1 || !listed[w.clusterB].Owned {
			t.Fatalf("%s before anything is shared: got = %+v, want only its own cluster", caller, listed)
		}
	}
	w.ok("staff", "PATCH", "/orgs/"+w.orgA+"/clusters/"+w.clusterA, map[string]any{"shared": true}, http.StatusOK, nil)

	for _, caller := range []string{"outsider", "foreign-key"} {
		listed, body := list(caller, w.orgB)
		shared, own := listed[w.clusterA], listed[w.clusterB]
		if len(listed) != 2 || !shared.Shared || shared.Owned || !shared.Connected {
			t.Fatalf("%s lists the shared cluster: got = %+v, want it shared, connected and not owned", caller, shared)
		}
		if strings.Contains(body, w.orgA) || strings.Contains(body, "id-of-a") {
			t.Fatalf("%s is shown the owner or the settings key of another org's cluster: %s", caller, body)
		}
		if !own.Owned || own.OrgID != w.orgB || own.ClusterID != "id-of-b" || own.Shared || own.Connected {
			t.Fatalf("%s lists its own cluster: got = %+v, want it owned by %s with its settings key", caller, own, w.orgB)
		}
	}
	if listed, _ := list("owner", w.orgA); len(listed) != 1 || !listed[w.clusterA].Owned || listed[w.clusterA].OrgID != w.orgA || listed[w.clusterA].ClusterID != "id-of-a" {
		t.Fatalf("the owner lists its shared cluster: got = %+v, want it owned with its settings key and nothing of org b", listed)
	}
	if listed, _ := list("staff", w.orgB); listed[w.clusterA].OrgID != w.orgA || listed[w.clusterA].ClusterID != "id-of-a" || listed[w.clusterA].Owned {
		t.Fatalf("staff list the clusters org b runs on: got = %+v, want the shared one with its owner", listed)
	}
	if rec := w.call("outsider", "POST", "/orgs/"+w.orgB+"/clusters", name{"name": w.clusterA}); rec.Code != http.StatusConflict {
		t.Fatalf("registering the name of a shared cluster: got = %d %s, want = 409", rec.Code, rec.Body)
	}

	w.ok("staff", "PATCH", "/orgs/"+w.orgA+"/clusters/"+w.clusterA, map[string]any{"shared": false}, http.StatusOK, nil)
	if listed, _ := list("outsider", w.orgB); len(listed) != 1 {
		t.Fatalf("after sharing ended: got = %+v, want only org b's own cluster", listed)
	}
}

func TestAnOrgThatDoesNotExistIsNotFoundForStaffToo(t *testing.T) {
	w := newWorld(t)
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"PATCH", "/orgs/org-missing", name{"name": "n"}},
		{"GET", "/orgs/org-missing/projects", nil},
		{"POST", "/orgs/org-missing/projects", name{"name": "n"}},
		{"GET", "/orgs/org-missing/api-keys", nil},
		{"POST", "/orgs/org-missing/api-keys", name{"name": "n"}},
		{"GET", "/orgs/org-missing/clusters", nil},
		{"POST", "/orgs/org-missing/clusters", name{"name": "n"}},
	} {
		for _, caller := range []string{"staff", "staff-user", "owner", "org-key"} {
			if rec := w.call(caller, c.method, c.path, c.body); rec.Code != http.StatusNotFound {
				t.Errorf("%s %s %s: got = %d %s, want = 404", caller, c.method, c.path, rec.Code, rec.Body)
			}
		}
	}
}

func TestOnlyStaffShareAClusterAndOnlyUnderItsOwnOrg(t *testing.T) {
	w := newWorld(t)
	var cluster ClusterView
	w.ok("staff", "PATCH", "/orgs/"+w.orgA+"/clusters/"+w.clusterA, map[string]any{"shared": true}, http.StatusOK, &cluster)
	if !cluster.Shared || cluster.OrgID != w.orgA {
		t.Fatalf("shared cluster: got = %+v, want it shared and still of %s", cluster, w.orgA)
	}
	w.ok("staff", "PATCH", "/orgs/"+w.orgA+"/clusters/"+w.clusterA, map[string]any{"shared": false}, http.StatusOK, &cluster)
	if cluster.Shared {
		t.Fatalf("unshared cluster: got = %+v, want it not shared", cluster)
	}
	for _, path := range []string{"/orgs/" + w.orgB + "/clusters/" + w.clusterA, "/orgs/" + w.orgA + "/clusters/never-registered"} {
		if rec := w.call("staff", "PATCH", path, map[string]any{"shared": true}); rec.Code != http.StatusNotFound {
			t.Fatalf("PATCH %s: got = %d %s, want = 404", path, rec.Code, rec.Body)
		}
	}
	if w.store.clusters[w.clusterA].Shared {
		t.Fatal("a refused request shared the cluster")
	}
}

func TestLineageFollowsMintedByThroughAnyDepth(t *testing.T) {
	keys := []*domain.APIKey{
		{ID: "root"}, {ID: "child", MintedBy: "root"}, {ID: "grandchild", MintedBy: "child"},
		{ID: "other"}, {ID: "others-child", MintedBy: "other"},
	}
	for root, want := range map[string]string{"root": "root,child,grandchild", "child": "child,grandchild", "grandchild": "grandchild", "other": "other,others-child", "missing": ""} {
		ids := []string{}
		for _, k := range lineage(keys, root) {
			ids = append(ids, k.ID)
		}
		if got := strings.Join(ids, ","); got != want {
			t.Errorf("lineage of %s: got = %q, want = %q", root, got, want)
		}
	}
}
