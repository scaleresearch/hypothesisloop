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
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth/authtest"
)

// tokenTable authenticates a token by looking it up, and refuses or fails on the rest.
type tokenTable map[string]auth.Principal

func (table tokenTable) Authenticate(_ context.Context, token string) (auth.Principal, error) {
	if token == "broken-directory" {
		return auth.Principal{}, errors.New("pq: database is down at 10.0.0.7")
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
	doc := New(r, "t", "1.0.0", "", everyKind, auth.NewAccess(authtest.Placement{}), zap.NewNop())
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
		if strings.Contains(recorder.Body.String(), "pq:") {
			t.Errorf("%s: body %q carries the platform's own error", name, recorder.Body.String())
		}
	}
}

// Once its platform experiment has closed, a coordinator token reads and files the summary.
func TestACoordinatorOfAClosedPlatformExperimentReadsAndFilesTheSummaryOnly(t *testing.T) {
	tokens := tokenTable{
		"open":   {Kind: auth.KindCoordinator, PlatformExperimentID: "pe-a"},
		"closed": {Kind: auth.KindCoordinator, PlatformExperimentID: "pe-a", PlatformExperimentClosed: true},
	}
	r := chi.NewRouter()
	doc := New(r, "t", "1.0.0", "", tokens, auth.NewAccess(authtest.Placement{}), zap.NewNop())
	ran := 0
	handler := func(context.Context, *struct{}) (*struct{}, error) {
		ran++
		return nil, nil
	}
	Register(doc, AudienceAgent, huma.Operation{OperationID: "read", Method: "GET", Path: "/read"}, handler)
	Register(doc, AudienceCoordinator, huma.Operation{OperationID: "write", Method: "POST", Path: "/write"}, handler)
	Register(doc, AudienceCoordinator, huma.Operation{OperationID: "patch", Method: "PATCH", Path: "/write"}, handler)
	Register(doc, AudienceCoordinator, huma.Operation{OperationID: "delete", Method: "DELETE", Path: "/write"}, handler)
	Register(doc, AudienceCoordinator, AfterClose(huma.Operation{OperationID: "summary", Method: "POST", Path: "/summary"}), handler)
	for _, tc := range []struct {
		token, method, path string
		want                int
	}{
		{"open", "GET", "/read", http.StatusNoContent}, {"open", "POST", "/write", http.StatusNoContent},
		{"open", "POST", "/summary", http.StatusNoContent},
		{"closed", "GET", "/read", http.StatusNoContent}, {"closed", "POST", "/summary", http.StatusNoContent},
		{"closed", "POST", "/write", http.StatusForbidden}, {"closed", "PATCH", "/write", http.StatusForbidden},
		{"closed", "DELETE", "/write", http.StatusForbidden},
	} {
		before := ran
		request := httptest.NewRequest(tc.method, tc.path, nil)
		request.Header.Set("Authorization", "Bearer "+tc.token)
		recorder := httptest.NewRecorder()
		r.ServeHTTP(recorder, request)
		if recorder.Code != tc.want || (ran > before) != (tc.want == http.StatusNoContent) {
			t.Errorf("%s %s as the %s coordinator: got = %d handler ran=%v, want = %d", tc.method, tc.path, tc.token, recorder.Code, ran > before, tc.want)
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
	doc := New(r, "t", "1.0.0", "", everyKind, auth.NewAccess(authtest.Placement{}), zap.NewNop())
	Register(doc, AudienceJob, huma.Operation{OperationID: "m", Method: "POST", Path: "/metrics-op", Summary: "s"},
		func(context.Context, *struct{}) (*struct{}, error) { return nil, nil })
	Register(doc, AudienceCoordinator, huma.Operation{OperationID: "c", Method: "POST", Path: "/coordinator-op", Summary: "s"},
		func(context.Context, *struct{}) (*struct{}, error) { return nil, nil })
	digest := doc.render(AudienceAgent)
	if !strings.Contains(digest, "/metrics-op") || strings.Contains(digest, "/coordinator-op") {
		t.Fatalf("agent digest:\n%s", digest)
	}
}

func TestARefusedKindIsNamedWithItsArticle(t *testing.T) {
	for kind, want := range map[auth.Kind]string{auth.KindAgent: "an agent", auth.KindCoordinator: "a coordinator"} {
		if got := withArticle(string(kind)); got != want {
			t.Errorf("withArticle(%q) = %q, want %q", kind, got, want)
		}
	}
}
