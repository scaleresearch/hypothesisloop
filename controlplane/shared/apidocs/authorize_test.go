package apidocs

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth/authtest"
)

// tokenTable authenticates a token by looking it up, and refuses or fails on the rest.
type tokenTable map[string]auth.Principal

func (table tokenTable) Authenticate(_ context.Context, token string) (auth.Principal, error) {
	if token == "broken-directory" {
		return auth.Principal{}, errors.New("database is down")
	}
	p, ok := table[token]
	if !ok {
		return auth.Principal{}, auth.Unauthenticated("unknown token")
	}
	return p, nil
}

var everyKind = tokenTable{
	"user":        {Kind: auth.KindUser, OrgIDs: []string{"org-a"}},
	"key":         {Kind: auth.KindKey, OrgIDs: []string{"org-a"}},
	"coordinator": {Kind: auth.KindCoordinator, PlatformExperimentID: "pe-a"},
	"agent":       {Kind: auth.KindAgent, PlatformExperimentID: "pe-a", AgentID: "agent-a"},
	"job":         {Kind: auth.KindJob, ExperimentID: "job-a"},
	"runtime-a":   {Kind: auth.KindRuntime, ClusterName: "cluster-a"},
	"launcher":    {Kind: auth.KindLauncher, LauncherName: "fly"},
	"staff":       {Kind: auth.KindStaff},
}

func audienceRouter(t *testing.T) (chi.Router, *auth.Principal) {
	t.Helper()
	r := chi.NewRouter()
	doc := New(r, "t", "1.0.0", "", everyKind, auth.NewAccess(authtest.Placement{}))
	var seen auth.Principal
	handler := func(ctx context.Context, _ *struct {
		Name string `path:"name"`
	}) (*struct{}, error) {
		seen, _ = auth.FromContext(ctx)
		return nil, nil
	}
	for path, audience := range map[string]Audience{
		"/agent/{name}": AudienceAgent, "/job/{name}": AudienceJob,
		"/coordinator/{name}": AudienceCoordinator, "/internal/{name}": AudienceInternal,
	} {
		Register(doc, audience, huma.Operation{OperationID: string(audience), Method: "GET", Path: path}, handler)
	}
	Register(doc, AudienceInternal, huma.Operation{OperationID: "internal-list", Method: "GET", Path: "/internal"},
		func(context.Context, *struct{}) (*struct{}, error) { return nil, nil })
	return r, &seen
}

func call(r chi.Router, path, authorization string) *httptest.ResponseRecorder {
	request := httptest.NewRequest("GET", path, nil)
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	recorder := httptest.NewRecorder()
	r.ServeHTTP(recorder, request)
	return recorder
}

func TestEachAudienceAdmitsExactlyItsCallers(t *testing.T) {
	r, _ := audienceRouter(t)
	admitted := map[string][]string{
		"/agent/x":            {"user", "key", "coordinator", "agent", "staff"},
		"/job/x":              {"user", "key", "coordinator", "agent", "job", "staff"},
		"/coordinator/x":      {"user", "key", "coordinator", "staff"},
		"/internal/cluster-a": {"runtime-a", "staff"},
		"/internal/cluster-b": {"staff"},
		"/internal":           {"staff"},
	}
	for path, allowed := range admitted {
		for token := range everyKind {
			want := http.StatusForbidden
			for _, kind := range allowed {
				if kind == token {
					want = http.StatusNoContent
				}
			}
			if got := call(r, path, "Bearer "+token).Code; got != want {
				t.Errorf("%s as %s: got = %d, want = %d", path, token, got, want)
			}
		}
	}
}

func TestARequestWithoutAUsableTokenNeverReachesTheHandler(t *testing.T) {
	r, seen := audienceRouter(t)
	for name, tc := range map[string]struct {
		authorization string
		want          int
	}{
		"no header":         {"", http.StatusUnauthorized},
		"not bearer":        {"Basic dXNlcjpwYXNz", http.StatusUnauthorized},
		"empty bearer":      {"Bearer ", http.StatusUnauthorized},
		"unknown token":     {"Bearer nobody", http.StatusUnauthorized},
		"directory failure": {"Bearer broken-directory", http.StatusInternalServerError},
	} {
		*seen = auth.Principal{}
		recorder := call(r, "/agent/x", tc.authorization)
		if recorder.Code != tc.want {
			t.Errorf("%s: got = %d, want = %d", name, recorder.Code, tc.want)
		}
		if seen.Kind != "" {
			t.Errorf("%s: the handler ran as %+v", name, *seen)
		}
		if !strings.Contains(recorder.Body.String(), `"error"`) {
			t.Errorf("%s: body %q is not the API's error envelope", name, recorder.Body.String())
		}
	}
}

func TestTheHandlerSeesThePrincipalTheTokenResolvedTo(t *testing.T) {
	r, seen := audienceRouter(t)
	if code := call(r, "/agent/x", "Bearer agent").Code; code != http.StatusNoContent {
		t.Fatalf("status: got = %d, want = 204", code)
	}
	if seen.Kind != auth.KindAgent || seen.AgentID != "agent-a" || seen.PlatformExperimentID != "pe-a" {
		t.Fatalf("principal in the handler's context: got = %+v, want the agent", *seen)
	}
}

func TestTheDocumentDeclaresBearerAuthOnEveryOperation(t *testing.T) {
	r, _ := audienceRouter(t)
	body := call(r, "/openapi.json", "").Body.String()
	if !strings.Contains(body, `"securitySchemes"`) || !strings.Contains(body, `"bearer"`) {
		t.Fatalf("openapi document does not declare the bearer scheme:\n%s", body)
	}
}

// The agent digest lists what an agent token can call, which includes the job audience's one
// operation and excludes everything coordinator-only.
func TestTheAgentDigestIncludesTheJobAudience(t *testing.T) {
	r, _ := audienceRouter(t)
	doc := New(r, "t", "1.0.0", "", everyKind, auth.NewAccess(authtest.Placement{}))
	Register(doc, AudienceJob, huma.Operation{OperationID: "m", Method: "POST", Path: "/metrics-op", Summary: "s"},
		func(context.Context, *struct{}) (*struct{}, error) { return nil, nil })
	Register(doc, AudienceCoordinator, huma.Operation{OperationID: "c", Method: "POST", Path: "/coordinator-op", Summary: "s"},
		func(context.Context, *struct{}) (*struct{}, error) { return nil, nil })
	digest := doc.render(AudienceAgent)
	if !strings.Contains(digest, "/metrics-op") || strings.Contains(digest, "/coordinator-op") {
		t.Fatalf("agent digest:\n%s", digest)
	}
}
