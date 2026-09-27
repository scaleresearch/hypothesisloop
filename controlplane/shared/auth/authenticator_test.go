package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/config"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// fakeDirectory is the rows a token is checked against.
type fakeDirectory struct {
	keys     map[string]*domain.APIKey
	pes      map[string]domain.Tenancy
	closedAt map[string]time.Time
	jobs     map[string]domain.Tenancy
	ended    map[string]bool
	cutAt    map[string]time.Time
	// answeredAt is when a planner answered its draft, by platform experiment and agent.
	answeredAt map[string]time.Time
	finishedAt map[string]time.Time
	// notSignedUp and launchers are read by the methods in roster_test.go.
	notSignedUp map[string]bool
	launchers   map[string]bool
	clusters    map[string]string
	staff       map[string]bool
	homes       map[string][]string
	names       map[string]string
	homeCreated []string
	fail        error
}

func (d *fakeDirectory) EnsureUserHome(_ context.Context, userID, name, orgName string) (*domain.User, []string, error) {
	if d.fail != nil {
		return nil, nil, d.fail
	}
	if _, ok := d.homes[userID]; !ok {
		d.homes[userID] = []string{"org-of-" + userID}
		d.homeCreated = append(d.homeCreated, orgName)
	}
	if name != "" {
		d.names[userID] = name
	}
	return &domain.User{ID: userID, Name: d.names[userID], Staff: d.staff[userID]}, d.homes[userID], nil
}

func (d *fakeDirectory) APIKeyByHash(_ context.Context, hash string) (*domain.APIKey, error) {
	return d.keys[hash], d.fail
}

func (d *fakeDirectory) PlatformExperimentTenancy(_ context.Context, id string) (*domain.Tenancy, *time.Time, error) {
	if t, ok := d.pes[id]; ok {
		return &t, at(d.closedAt, id), d.fail
	}
	return nil, nil, d.fail
}

func at(times map[string]time.Time, key string) *time.Time {
	if when, ok := times[key]; ok {
		return &when
	}
	return nil
}

func (d *fakeDirectory) ExperimentTenancy(_ context.Context, id string) (*domain.Tenancy, bool, error) {
	if t, ok := d.jobs[id]; ok {
		return &t, d.ended[id], d.fail
	}
	return nil, false, d.fail
}

func (d *fakeDirectory) AgentCutAt(_ context.Context, pe, agent string) (*time.Time, error) {
	return at(d.cutAt, pe+"/"+agent), d.fail
}

func (d *fakeDirectory) PlannerAnsweredAt(_ context.Context, pe, agent string) (*time.Time, error) {
	return at(d.answeredAt, pe+"/"+agent), d.fail
}

func (d *fakeDirectory) AgentFinishedAt(_ context.Context, pe, agent string) (*time.Time, error) {
	return at(d.finishedAt, pe+"/"+agent), d.fail
}

func (d *fakeDirectory) ClusterOrg(_ context.Context, name string) (string, bool, error) {
	org, ok := d.clusters[name]
	return org, ok, d.fail
}

var tenancyA = domain.Tenancy{PlatformExperimentID: "pe-a", ProjectID: "prj-a", OrgID: "org-a"}

func newTestAuthenticator(t *testing.T, signers ...testSigner) (*Authenticator, *fakeDirectory) {
	t.Helper()
	directory := &fakeDirectory{
		keys:       map[string]*domain.APIKey{},
		pes:        map[string]domain.Tenancy{"pe-a": tenancyA},
		closedAt:   map[string]time.Time{},
		jobs:       map[string]domain.Tenancy{"job-a": tenancyA},
		ended:      map[string]bool{},
		cutAt:      map[string]time.Time{},
		answeredAt: map[string]time.Time{},
		finishedAt: map[string]time.Time{},
		clusters:   map[string]string{"cluster-a": "org-a"},
		staff:      map[string]bool{},
		homes:      map[string][]string{},
		names:      map[string]string{},
	}
	cfg := config.AuthConfig{SigningSecret: testSecret}
	if len(signers) > 0 {
		provider := config.IdentityProvider{Issuer: testIssuer, Audience: testAudience}
		for _, s := range signers {
			provider.JWTKeys = append(provider.JWTKeys, s.jwk)
		}
		cfg.IdentityProviders = []config.IdentityProvider{provider}
	}
	a, err := NewAuthenticator(cfg, directory)
	if err != nil {
		t.Fatalf("new authenticator: %v", err)
	}
	a.now = func() time.Time { return testNow }
	return a, directory
}

func assertRefused(t *testing.T, what string, p Principal, err error) {
	t.Helper()
	var refusal *Error
	if !errors.As(err, &refusal) || refusal.GetStatus() != http.StatusUnauthorized {
		t.Errorf("%s: got = %+v, %v, want a 401 refusal", what, p, err)
	}
}

func TestEachDerivedKindResolvesWhileItsRowStands(t *testing.T) {
	a, _ := newTestAuthenticator(t)
	ctx := context.Background()
	inA := func(p Principal) Principal {
		p.PlatformExperimentID, p.ProjectID, p.OrgIDs = "pe-a", "prj-a", []string{"org-a"}
		return p
	}
	for _, tc := range []struct {
		scope string
		want  Principal
	}{
		{StaffScope, Principal{Kind: KindStaff}},
		{RuntimeScope("cluster-a"), Principal{Kind: KindRuntime, ClusterName: "cluster-a", OrgIDs: []string{"org-a"}}},
		{JobScope("job-a"), inA(Principal{Kind: KindJob, ExperimentID: "job-a"})},
		{AgentScope("pe-a", "agent-1"), inA(Principal{Kind: KindAgent, AgentID: "agent-1"})},
		{CoordinatorScope("pe-a"), inA(Principal{Kind: KindCoordinator})},
	} {
		got, err := a.Authenticate(ctx, mustMint(t, testSecret, tc.scope))
		if err != nil || !samePrincipal(got, tc.want) {
			t.Errorf("%s: got = %+v, %v, want = %+v", tc.scope, got, err, tc.want)
		}
	}
}

func TestADerivedTokenStopsWorkingWhenItsRowSaysSo(t *testing.T) {
	ctx := context.Background()
	woundDown := testNow.Add(-domain.AgentWindDown)
	for name, tc := range map[string]struct {
		scope  string
		change func(*fakeDirectory)
	}{
		"runtime of an unregistered cluster":                          {RuntimeScope("cluster-typo"), func(*fakeDirectory) {}},
		"job that has ended":                                          {JobScope("job-a"), func(d *fakeDirectory) { d.ended["job-a"] = true }},
		"job that does not exist":                                     {JobScope("job-missing"), func(*fakeDirectory) {}},
		"agent of a platform experiment closed a wind-down ago":       {AgentScope("pe-a", "agent-1"), func(d *fakeDirectory) { d.closedAt["pe-a"] = woundDown }},
		"agent of a missing platform experiment":                      {AgentScope("pe-missing", "agent-1"), func(*fakeDirectory) {}},
		"agent that was cut a wind-down ago":                          {AgentScope("pe-a", "agent-1"), func(d *fakeDirectory) { d.cutAt["pe-a/agent-1"] = woundDown }},
		"agent cut long ago in a platform experiment just closed":     {AgentScope("pe-a", "agent-1"), func(d *fakeDirectory) { d.cutAt["pe-a/agent-1"] = woundDown; d.closedAt["pe-a"] = testNow }},
		"planner that answered its draft a wind-down ago":             {AgentScope("pe-a", "agent-1"), func(d *fakeDirectory) { d.answeredAt["pe-a/agent-1"] = woundDown }},
		"agent that finished a wind-down ago":                         {AgentScope("pe-a", "agent-1"), func(d *fakeDirectory) { d.finishedAt["pe-a/agent-1"] = woundDown }},
		"coordinator of a platform experiment closed a wind-down ago": {CoordinatorScope("pe-a"), func(d *fakeDirectory) { d.closedAt["pe-a"] = woundDown }},
		"coordinator of a missing platform experiment":                {CoordinatorScope("pe-missing"), func(*fakeDirectory) {}},
	} {
		a, directory := newTestAuthenticator(t)
		token := mustMint(t, testSecret, tc.scope)
		tc.change(directory)
		p, err := a.Authenticate(ctx, token)
		assertRefused(t, name, p, err)
	}

	a, directory := newTestAuthenticator(t)
	directory.cutAt["pe-a/agent-2"], directory.finishedAt["pe-a/agent-2"] = woundDown, woundDown
	if _, err := a.Authenticate(ctx, mustMint(t, testSecret, AgentScope("pe-a", "agent-1"))); err != nil {
		t.Errorf("an agent whose rival was cut and finished: got = %v, want it still admitted", err)
	}
}

func TestAnAgentTokenOutlivesTheEndOfItsRunByTheWindDownOnly(t *testing.T) {
	ctx := context.Background()
	lastInstant := testNow.Add(-domain.AgentWindDown + time.Second)
	for name, change := range map[string]func(*fakeDirectory){
		"platform experiment closed this instant":         func(d *fakeDirectory) { d.closedAt["pe-a"] = testNow },
		"platform experiment closed a second short of it": func(d *fakeDirectory) { d.closedAt["pe-a"] = lastInstant },
		"agent cut this instant":                          func(d *fakeDirectory) { d.cutAt["pe-a/agent-1"] = testNow },
		"agent cut a second short of it":                  func(d *fakeDirectory) { d.cutAt["pe-a/agent-1"] = lastInstant },
		"planner answered a second short of it":           func(d *fakeDirectory) { d.answeredAt["pe-a/agent-1"] = lastInstant },
		"agent finished this instant":                     func(d *fakeDirectory) { d.finishedAt["pe-a/agent-1"] = testNow },
		"agent finished a second short of it":             func(d *fakeDirectory) { d.finishedAt["pe-a/agent-1"] = lastInstant },
	} {
		a, directory := newTestAuthenticator(t)
		change(directory)
		p, err := a.Authenticate(ctx, mustMint(t, testSecret, AgentScope("pe-a", "agent-1")))
		if err != nil || p.Kind != KindAgent || p.PlatformExperimentClosed {
			t.Errorf("%s: got = %+v, %v, want the agent admitted", name, p, err)
		}
	}
}

// A coordinator writes its closing summary after the close, in the wind-down its machine is kept
// for. Its token says the experiment is closed for as long as it works, and works no longer.
func TestACoordinatorTokenOutlivesCloseByTheWindDownOnlyAndSaysSo(t *testing.T) {
	a, directory := newTestAuthenticator(t)
	ctx := context.Background()
	token := mustMint(t, testSecret, CoordinatorScope("pe-a"))
	open, err := a.Authenticate(ctx, token)
	if err != nil || open.PlatformExperimentClosed {
		t.Fatalf("while open: got = %+v, %v, want it admitted and not marked closed", open, err)
	}
	for name, closedAt := range map[string]time.Time{
		"closed this instant":                  testNow,
		"closed a second short of a wind-down": testNow.Add(-domain.AgentWindDown + time.Second),
	} {
		directory.closedAt["pe-a"] = closedAt
		closed, err := a.Authenticate(ctx, token)
		if err != nil || closed.Kind != KindCoordinator || !closed.PlatformExperimentClosed {
			t.Fatalf("%s: got = %+v, %v, want it admitted and marked closed", name, closed, err)
		}
	}
	directory.closedAt["pe-a"] = testNow.Add(-domain.AgentWindDown)
	p, err := a.Authenticate(ctx, token)
	assertRefused(t, "a wind-down after the close", p, err)
}

func TestForgedTokensAreRefusedBeforeAnyRowIsRead(t *testing.T) {
	a, directory := newTestAuthenticator(t)
	directory.fail = errors.New("the directory must not be read for a forged token")
	ctx := context.Background()
	for name, token := range map[string]string{
		"staff token of another deployment": mustMint(t, "another-signing-secret-of-enough-length", StaffScope),
		"staff scope with no mac":           derivedPrefix + encoding.EncodeToString([]byte(StaffScope)) + ".",
		"bare prefix":                       derivedPrefix,
		"jwt with no keys configured":       "aaa.bbb.ccc",
		"empty":                             "",
	} {
		p, err := a.Authenticate(ctx, token)
		assertRefused(t, name, p, err)
	}
}

func TestAnAPIKeyResolvesToItsOrgUntilRevoked(t *testing.T) {
	a, directory := newTestAuthenticator(t)
	ctx := context.Background()
	_, orgToken, _ := NewAPIKey()
	_, projectToken, _ := NewAPIKey()
	_, revokedToken, _ := NewAPIKey()
	_, unknownToken, _ := NewAPIKey()
	_, orphanedToken, _ := NewAPIKey()
	revokedAt := testNow
	directory.keys[HashAPIKey(orphanedToken)] = &domain.APIKey{ID: "k4", OrgID: "org-a", Orphaned: true}
	directory.keys[HashAPIKey(orgToken)] = &domain.APIKey{ID: "k1", OrgID: "org-a"}
	directory.keys[HashAPIKey(projectToken)] = &domain.APIKey{ID: "k2", OrgID: "org-a", ProjectID: "prj-a"}
	directory.keys[HashAPIKey(revokedToken)] = &domain.APIKey{ID: "k3", OrgID: "org-a", RevokedAt: &revokedAt}

	got, err := a.Authenticate(ctx, orgToken)
	if err != nil || got.KeyID != "k1" || !samePrincipal(got, Principal{Kind: KindKey, OrgIDs: []string{"org-a"}}) {
		t.Errorf("org key: got = %+v, %v", got, err)
	}
	got, err = a.Authenticate(ctx, projectToken)
	if err != nil || got.KeyID != "k2" || !samePrincipal(got, Principal{Kind: KindKey, OrgIDs: []string{"org-a"}, ProjectID: "prj-a"}) {
		t.Errorf("project key: got = %+v, %v", got, err)
	}
	p, err := a.Authenticate(ctx, revokedToken)
	assertRefused(t, "revoked key", p, err)
	p, err = a.Authenticate(ctx, orphanedToken)
	assertRefused(t, "a key whose creator no longer administers its org", p, err)
	p, err = a.Authenticate(ctx, unknownToken)
	assertRefused(t, "unknown key", p, err)
	p, err = a.Authenticate(ctx, orgToken+"x")
	assertRefused(t, "a key with one character changed", p, err)
}

func TestAUsersFirstRequestMakesTheirHomeAndLaterOnesReuseIt(t *testing.T) {
	ec := newECSigner(t, "ec-1")
	a, directory := newTestAuthenticator(t, ec)
	ctx := context.Background()

	first, err := a.Authenticate(ctx, ec.token(t, goodClaims()))
	want := Principal{Kind: KindUser, UserID: "user-1", Name: "Ada Lovelace", OrgIDs: []string{"org-of-user-1"}}
	if err != nil || !samePrincipal(first, want) {
		t.Fatalf("first request: got = %+v, %v, want = %+v", first, err, want)
	}
	if _, err := a.Authenticate(ctx, ec.token(t, goodClaims())); err != nil {
		t.Fatalf("second request: %v", err)
	}
	if len(directory.homeCreated) != 1 || directory.homeCreated[0] != domain.DefaultOrgName {
		t.Fatalf("homes created: got = %v, want one named %q", directory.homeCreated, domain.DefaultOrgName)
	}

	// The home org is named the same whatever the person's claims say of them.
	nameless := goodClaims()
	delete(nameless, "name")
	nameless["sub"] = "user-2"
	loud := goodClaims()
	loud["sub"] = "user-3"
	loud["name"] = "Eve\x00\n" + strings.Repeat("x", 500)
	loud["email"] = "eve@example.invalid"
	loud["user_name"] = "eve-on-github"
	for i, claims := range []map[string]any{nameless, loud} {
		p, err := a.Authenticate(ctx, ec.token(t, claims))
		if err != nil {
			t.Fatalf("first request of %v: %v", claims["sub"], err)
		}
		// The name kept for a person is their claim held to what a name may be, and it is the
		// one the principal carries.
		if p.Name != directory.names[p.UserID] || (p.Name != "" && (len(p.Name) != domain.MaxNameLength || !strings.HasPrefix(p.Name, "Evexxx"))) {
			t.Fatalf("name of %v: got = %q, kept %q, want the claim cleaned and cut to %d bytes", claims["sub"], p.Name, directory.names[p.UserID], domain.MaxNameLength)
		}
		if got := directory.homeCreated[i+1]; got != domain.DefaultOrgName {
			t.Fatalf("home org of %v: got = %q, want = %q", claims["sub"], got, domain.DefaultOrgName)
		}
	}
	if cleaned, err := domain.ValidateName("org", domain.DefaultOrgName); err != nil || cleaned != domain.DefaultOrgName {
		t.Fatalf("the home org's name is one the API would refuse: %q, %v", cleaned, err)
	}

	directory.staff["user-1"] = true
	staff, err := a.Authenticate(ctx, ec.token(t, goodClaims()))
	if err != nil || staff.Kind != KindStaff || staff.UserID != "user-1" {
		t.Fatalf("a staff user: got = %+v, %v, want kind staff", staff, err)
	}

	for name, change := range map[string]func(map[string]any){
		"expired jwt":             func(c map[string]any) { c["exp"] = testNow.Add(-time.Minute).Unix() },
		"jwt for another service": func(c map[string]any) { c["aud"] = "another-service" },
		"jwt not valid yet":       func(c map[string]any) { c["nbf"] = testNow.Add(time.Minute).Unix() },
		"anonymous session":       func(c map[string]any) { c["is_anonymous"] = true },
	} {
		claims := goodClaims()
		change(claims)
		p, err := a.Authenticate(ctx, ec.token(t, claims))
		assertRefused(t, name, p, err)
	}
}

func TestAnActorIsTheCredentialAndNothingARequestSays(t *testing.T) {
	for name, tc := range map[string]struct {
		p    Principal
		want domain.Actor
	}{
		"a signed-in person":  {Principal{Kind: KindUser, UserID: "u-1", Name: "Ada"}, domain.Actor{Kind: domain.ActorUser, ID: "u-1", Name: "Ada"}},
		"staff who signed in": {Principal{Kind: KindStaff, UserID: "u-2", Name: "Grace"}, domain.Actor{Kind: domain.ActorUser, ID: "u-2", Name: "Grace"}},
		"an api key":          {Principal{Kind: KindKey, KeyID: "key-1"}, domain.Actor{Kind: domain.ActorKey, ID: "key-1"}},
		"an agent":            {Principal{Kind: KindAgent, AgentID: "agent-1", PlatformExperimentID: "pe-1"}, domain.Actor{Kind: domain.ActorAgent, ID: "agent-1"}},
		"an agent's job":      {Principal{Kind: KindJob, AgentID: "agent-1", ExperimentID: "exp-1"}, domain.Actor{Kind: domain.ActorAgent, ID: "agent-1"}},
		"a coordinator":       {Principal{Kind: KindCoordinator, PlatformExperimentID: "pe-1"}, domain.Actor{Kind: domain.ActorCoordinator, ID: "pe-1"}},
		"a staff token":       {Principal{Kind: KindStaff}, domain.Actor{Kind: domain.ActorStaff}},
		"a runtime":           {Principal{Kind: KindRuntime, ClusterName: "c"}, domain.Actor{}},
		"a launcher":          {Principal{Kind: KindLauncher, LauncherName: "l"}, domain.Actor{}},
	} {
		if got := tc.p.Actor(); got != tc.want {
			t.Errorf("%s: got = %+v, want = %+v", name, got, tc.want)
		}
	}
}

func TestADirectoryFailureIsNotARefusal(t *testing.T) {
	a, directory := newTestAuthenticator(t)
	directory.fail = errors.New("database is down")
	_, err := a.Authenticate(context.Background(), mustMint(t, testSecret, JobScope("job-a")))
	var refusal *Error
	if err == nil || errors.As(err, &refusal) {
		t.Fatalf("got = %v, want the directory's own error, which answers 500", err)
	}
}

func TestAuthenticatorRefusesAMisconfiguredIdentityProvider(t *testing.T) {
	ec := newECSigner(t, "ec-1")
	provider := config.IdentityProvider{Issuer: testIssuer, Audience: testAudience, JWTKeys: []config.JWTKey{ec.jwk, ec.jwk}}
	if _, err := NewAuthenticator(config.AuthConfig{SigningSecret: testSecret, IdentityProviders: []config.IdentityProvider{provider}}, &fakeDirectory{}); err == nil {
		t.Error("the same kid listed twice: got = nil, want an error")
	}
}

func TestBearerToken(t *testing.T) {
	if token, err := BearerToken("Bearer abc"); err != nil || token != "abc" {
		t.Errorf("got = %q, %v, want abc", token, err)
	}
	for _, header := range []string{"", "abc", "Bearer ", "Basic abc", "bearer abc"} {
		if token, err := BearerToken(header); err == nil {
			t.Errorf("header %q: got token %q, want a refusal", header, token)
		}
	}
}

func TestOnlyARuntimeOrStaffMayPush(t *testing.T) {
	a, _ := newTestAuthenticator(t)
	served := false
	var caller Principal
	handler := a.RequireRuntime(zap.NewNop(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served = true
		caller, _ = FromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, tc := range []struct {
		name   string
		header string
		want   int
	}{
		{"no token", "", http.StatusUnauthorized},
		{"registered runtime", "Bearer " + mustMint(t, testSecret, RuntimeScope("cluster-a")), http.StatusNoContent},
		{"staff", "Bearer " + mustMint(t, testSecret, StaffScope), http.StatusNoContent},
		{"unregistered runtime", "Bearer " + mustMint(t, testSecret, RuntimeScope("cluster-typo")), http.StatusUnauthorized},
		{"agent", "Bearer " + mustMint(t, testSecret, AgentScope("pe-a", "agent-1")), http.StatusForbidden},
		{"job", "Bearer " + mustMint(t, testSecret, JobScope("job-a")), http.StatusForbidden},
		{"forged runtime", "Bearer " + mustMint(t, "another-signing-secret-of-enough-length", RuntimeScope("cluster-a")), http.StatusUnauthorized},
	} {
		served = false
		request := httptest.NewRequest(http.MethodPost, "/internal/node-metrics", strings.NewReader("{}"))
		if tc.header != "" {
			request.Header.Set("Authorization", tc.header)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != tc.want || served != (tc.want == http.StatusNoContent) {
			t.Errorf("%s: got = %d served=%v, want = %d", tc.name, recorder.Code, served, tc.want)
		}
		if tc.want != http.StatusNoContent && !strings.Contains(recorder.Body.String(), `"error"`) {
			t.Errorf("%s: refusal body %q is not the API's error envelope", tc.name, recorder.Body.String())
		}
		if tc.name == "registered runtime" && (caller.Kind != KindRuntime || caller.ClusterName != "cluster-a") {
			t.Errorf("%s: the handler was given %+v, want the runtime of cluster-a", tc.name, caller)
		}
	}
}

func TestOnlyStaffReachWhatRequireStaffGuards(t *testing.T) {
	a, _ := newTestAuthenticator(t)
	handler := a.RequireStaff(zap.NewNop(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	for name, tc := range map[string]struct {
		header string
		want   int
	}{
		"no token": {"", http.StatusUnauthorized},
		"staff":    {"Bearer " + mustMint(t, testSecret, StaffScope), http.StatusOK},
		"runtime":  {"Bearer " + mustMint(t, testSecret, RuntimeScope("cluster-a")), http.StatusForbidden},
		"agent":    {"Bearer " + mustMint(t, testSecret, AgentScope("pe-a", "agent-1")), http.StatusForbidden},
	} {
		request := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		if tc.header != "" {
			request.Header.Set("Authorization", tc.header)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != tc.want {
			t.Errorf("%s: got = %d, want = %d", name, recorder.Code, tc.want)
		}
	}
}

func TestAFailureOfThePlatformsOwnIsAnsweredWithoutItsText(t *testing.T) {
	a, directory := newTestAuthenticator(t)
	directory.fail = errors.New("pq: password authentication failed for user hl at 10.0.0.7")
	handler := a.RequireRuntime(zap.NewNop(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the handler ran behind a failed directory read")
	}))
	request := httptest.NewRequest(http.MethodPost, "/internal/node-metrics", strings.NewReader("{}"))
	request.Header.Set("Authorization", "Bearer "+mustMint(t, testSecret, RuntimeScope("cluster-a")))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusInternalServerError || strings.Contains(recorder.Body.String(), "pq:") || !strings.Contains(recorder.Body.String(), InternalError) {
		t.Fatalf("got = %d %q, want 500 with the generic body", recorder.Code, recorder.Body.String())
	}
}

func TestInternalLogsTheCauseAndAnswersWithoutIt(t *testing.T) {
	core, logs := observer.New(zap.ErrorLevel)
	cause := errors.New(`pq: relation "signups" does not exist`)
	answer := Internal(zap.New(core), "list agents", cause, zap.String("id", "pe-1"))
	body, err := json.Marshal(answer)
	if err != nil {
		t.Fatal(err)
	}
	if answer.GetStatus() != http.StatusInternalServerError || string(body) != `{"error":"internal error"}` {
		t.Fatalf("answered %d %s, want 500 with the generic body", answer.GetStatus(), body)
	}
	entries := logs.All()
	if len(entries) != 1 || entries[0].Message != "list agents" || entries[0].ContextMap()["id"] != "pe-1" || entries[0].ContextMap()["error"] != cause.Error() {
		t.Fatalf("logged %+v, want the cause once with the request's fields", entries)
	}
}

// A runtime branches on one refusal: its cluster is gone, so it stops what it runs and exits.
// A token that fails for any other cause carries no reason, and the runtime keeps its jobs.
func TestOnlyARemovedClustersRuntimeIsToldItsClusterIsNotRegistered(t *testing.T) {
	a, _ := newTestAuthenticator(t)
	handler := a.RequireRuntime(zap.NewNop(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	for name, tc := range map[string]struct {
		token  string
		reason string
	}{
		"removed cluster": {mustMint(t, testSecret, RuntimeScope("cluster-typo")), domain.ReasonClusterNotRegistered},
		"forged token":    {mustMint(t, "another-signing-secret-of-enough-length", RuntimeScope("cluster-a")), ""},
	} {
		request := httptest.NewRequest(http.MethodPost, "/internal/node-metrics", strings.NewReader("{}"))
		request.Header.Set("Authorization", "Bearer "+tc.token)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		var body struct {
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: %v in %s", name, err, recorder.Body)
		}
		if recorder.Code != http.StatusUnauthorized || body.Reason != tc.reason {
			t.Errorf("%s: status = %d reason = %q, want 401 and %q", name, recorder.Code, body.Reason, tc.reason)
		}
	}
}
