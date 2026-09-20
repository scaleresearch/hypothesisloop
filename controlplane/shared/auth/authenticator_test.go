package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/config"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// fakeDirectory is the rows a token is checked against.
type fakeDirectory struct {
	keys        map[string]*domain.APIKey
	pes         map[string]domain.Tenancy
	closed      map[string]bool
	jobs        map[string]domain.Tenancy
	ended       map[string]bool
	cut         map[string]bool
	clusters    map[string]string
	staff       map[string]bool
	homes       map[string][]string
	homeCreated []string
	fail        error
}

func (d *fakeDirectory) EnsureUserHome(_ context.Context, userID, orgName string) (*domain.User, []string, error) {
	if d.fail != nil {
		return nil, nil, d.fail
	}
	if _, ok := d.homes[userID]; !ok {
		d.homes[userID] = []string{"org-of-" + userID}
		d.homeCreated = append(d.homeCreated, orgName)
	}
	return &domain.User{ID: userID, Staff: d.staff[userID]}, d.homes[userID], nil
}

func (d *fakeDirectory) APIKeyByHash(_ context.Context, hash string) (*domain.APIKey, error) {
	return d.keys[hash], d.fail
}

func (d *fakeDirectory) PlatformExperimentTenancy(_ context.Context, id string) (*domain.Tenancy, bool, error) {
	if t, ok := d.pes[id]; ok {
		return &t, d.closed[id], d.fail
	}
	return nil, false, d.fail
}

func (d *fakeDirectory) ExperimentTenancy(_ context.Context, id string) (*domain.Tenancy, bool, error) {
	if t, ok := d.jobs[id]; ok {
		return &t, d.ended[id], d.fail
	}
	return nil, false, d.fail
}

func (d *fakeDirectory) IsAgentCut(_ context.Context, pe, agent string) (bool, error) {
	return d.cut[pe+"/"+agent], d.fail
}

func (d *fakeDirectory) ClusterOrg(_ context.Context, name string) (string, bool, error) {
	org, ok := d.clusters[name]
	return org, ok, d.fail
}

var tenancyA = domain.Tenancy{PlatformExperimentID: "pe-a", ProjectID: "prj-a", OrgID: "org-a"}

func newTestAuthenticator(t *testing.T, signers ...testSigner) (*Authenticator, *fakeDirectory) {
	t.Helper()
	directory := &fakeDirectory{
		keys:     map[string]*domain.APIKey{},
		pes:      map[string]domain.Tenancy{"pe-a": tenancyA},
		closed:   map[string]bool{},
		jobs:     map[string]domain.Tenancy{"job-a": tenancyA},
		ended:    map[string]bool{},
		cut:      map[string]bool{},
		clusters: map[string]string{"cluster-a": "org-a"},
		staff:    map[string]bool{},
		homes:    map[string][]string{},
	}
	cfg := config.AuthConfig{SigningSecret: testSecret, Issuer: testIssuer}
	for _, s := range signers {
		cfg.JWTKeys = append(cfg.JWTKeys, s.jwk)
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
	for name, tc := range map[string]struct {
		scope  string
		change func(*fakeDirectory)
	}{
		"runtime of an unregistered cluster":           {RuntimeScope("cluster-typo"), func(*fakeDirectory) {}},
		"job that has ended":                           {JobScope("job-a"), func(d *fakeDirectory) { d.ended["job-a"] = true }},
		"job that does not exist":                      {JobScope("job-missing"), func(*fakeDirectory) {}},
		"agent of a closed platform experiment":        {AgentScope("pe-a", "agent-1"), func(d *fakeDirectory) { d.closed["pe-a"] = true }},
		"agent of a missing platform experiment":       {AgentScope("pe-missing", "agent-1"), func(*fakeDirectory) {}},
		"agent that was cut":                           {AgentScope("pe-a", "agent-1"), func(d *fakeDirectory) { d.cut["pe-a/agent-1"] = true }},
		"coordinator of a closed platform experiment":  {CoordinatorScope("pe-a"), func(d *fakeDirectory) { d.closed["pe-a"] = true }},
		"coordinator of a missing platform experiment": {CoordinatorScope("pe-missing"), func(*fakeDirectory) {}},
	} {
		a, directory := newTestAuthenticator(t)
		token := mustMint(t, testSecret, tc.scope)
		tc.change(directory)
		p, err := a.Authenticate(ctx, token)
		assertRefused(t, name, p, err)
	}

	a, directory := newTestAuthenticator(t)
	directory.cut["pe-a/agent-2"] = true
	if _, err := a.Authenticate(ctx, mustMint(t, testSecret, AgentScope("pe-a", "agent-1"))); err != nil {
		t.Errorf("an agent whose rival was cut: got = %v, want it still admitted", err)
	}
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
	revokedAt := testNow
	directory.keys[HashAPIKey(orgToken)] = &domain.APIKey{ID: "k1", OrgID: "org-a"}
	directory.keys[HashAPIKey(projectToken)] = &domain.APIKey{ID: "k2", OrgID: "org-a", ProjectID: "prj-a"}
	directory.keys[HashAPIKey(revokedToken)] = &domain.APIKey{ID: "k3", OrgID: "org-a", RevokedAt: &revokedAt}

	got, err := a.Authenticate(ctx, orgToken)
	if err != nil || !samePrincipal(got, Principal{Kind: KindKey, OrgIDs: []string{"org-a"}}) {
		t.Errorf("org key: got = %+v, %v", got, err)
	}
	got, err = a.Authenticate(ctx, projectToken)
	if err != nil || !samePrincipal(got, Principal{Kind: KindKey, OrgIDs: []string{"org-a"}, ProjectID: "prj-a"}) {
		t.Errorf("project key: got = %+v, %v", got, err)
	}
	p, err := a.Authenticate(ctx, revokedToken)
	assertRefused(t, "revoked key", p, err)
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
	if len(directory.homeCreated) != 1 || directory.homeCreated[0] != "Ada Lovelace" {
		t.Fatalf("homes created: got = %v, want one named after the user", directory.homeCreated)
	}

	nameless := goodClaims()
	delete(nameless, "name")
	nameless["sub"] = "user-2"
	if _, err := a.Authenticate(ctx, ec.token(t, nameless)); err != nil {
		t.Fatalf("a user with no name claim: %v", err)
	}
	if directory.homeCreated[1] != "user-2" {
		t.Fatalf("org name for a user with no name claim: got = %q, want the subject", directory.homeCreated[1])
	}

	directory.staff["user-1"] = true
	staff, err := a.Authenticate(ctx, ec.token(t, goodClaims()))
	if err != nil || staff.Kind != KindStaff || staff.UserID != "user-1" {
		t.Fatalf("a staff user: got = %+v, %v, want kind staff", staff, err)
	}

	expired := goodClaims()
	expired["exp"] = testNow.Add(-time.Minute).Unix()
	p, err := a.Authenticate(ctx, ec.token(t, expired))
	assertRefused(t, "expired jwt", p, err)
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

func TestAuthenticatorRefusesAMisconfiguredKey(t *testing.T) {
	ec := newECSigner(t, "ec-1")
	if _, err := NewAuthenticator(config.AuthConfig{SigningSecret: testSecret, JWTKeys: []config.JWTKey{ec.jwk, ec.jwk}}, &fakeDirectory{}); err == nil {
		t.Error("the same kid listed twice: got = nil, want an error")
	}
	if _, err := NewAuthenticator(config.AuthConfig{SigningSecret: testSecret, JWTKeys: []config.JWTKey{{Kid: "k", Kty: "oct"}}}, &fakeDirectory{}); err == nil {
		t.Error("a symmetric key: got = nil, want an error")
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
	handler := a.RequireRuntime(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served = true
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
	}
}
