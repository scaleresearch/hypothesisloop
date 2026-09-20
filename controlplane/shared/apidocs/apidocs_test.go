package apidocs

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth/authtest"
	"go.uber.org/zap"
)

func TestRegisterRequiresValidAudience(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected Register to panic on an invalid audience")
		}
	}()
	r := chi.NewRouter()
	doc := New(r, "t", "1.0.0", "", authtest.Staff, auth.NewAccess(authtest.Placement{}), zap.NewNop())
	Register(doc, Audience("bogus"), huma.Operation{OperationID: "x", Method: "GET", Path: "/x"},
		func(context.Context, *struct{}) (*struct{}, error) { return nil, nil })
}

func TestMountExploreAudienceFiltersByAudience(t *testing.T) {
	r := chi.NewRouter()
	doc := New(r, "t", "1.0.0", "", authtest.Staff, auth.NewAccess(authtest.Placement{}), zap.NewNop())
	Register(doc, AudienceAgent, huma.Operation{OperationID: "agent-op", Method: "GET", Path: "/agent-only", Summary: "agent summary"},
		func(context.Context, *struct{}) (*struct{}, error) { return nil, nil })
	Register(doc, AudienceCoordinator, huma.Operation{OperationID: "coord-op", Method: "GET", Path: "/coord-only", Summary: "coordinator summary"},
		func(context.Context, *struct{}) (*struct{}, error) { return nil, nil })

	doc.MountExploreAudience(r, "/explore", AudienceAgent)
	doc.MountExploreAudience(r, "/explore/coordinator", AudienceCoordinator)

	get := func(path string) string {
		req := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Body.String()
	}

	agentDigest := get("/explore")
	if !strings.Contains(agentDigest, "/agent-only") {
		t.Fatalf("agent digest missing its own operation:\n%s", agentDigest)
	}
	if strings.Contains(agentDigest, "/coord-only") {
		t.Fatalf("agent digest leaked a coordinator-only operation:\n%s", agentDigest)
	}

	coordDigest := get("/explore/coordinator")
	if !strings.Contains(coordDigest, "/coord-only") {
		t.Fatalf("coordinator digest missing its own operation:\n%s", coordDigest)
	}
	if strings.Contains(coordDigest, "/agent-only") {
		t.Fatalf("coordinator digest leaked an agent-only operation:\n%s", coordDigest)
	}
}

func TestMountRulesServesThePreambleWithoutOperations(t *testing.T) {
	r := chi.NewRouter()
	doc := New(r, "t", "1.0.0", PlatformRules, authtest.Staff, auth.NewAccess(authtest.Placement{}), zap.NewNop())
	Register(doc, AudienceAgent, huma.Operation{OperationID: "agent-op", Method: "GET", Path: "/agent-only", Summary: "agent summary"},
		func(context.Context, *struct{}) (*struct{}, error) { return nil, nil })
	doc.MountRules(r, "/explore/rules")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/explore/rules", nil))
	if w.Code != 200 || w.Body.String() != PlatformRules {
		t.Fatalf("code = %d, body:\n%s", w.Code, w.Body.String())
	}
	// GET /platform-experiments/{id}/results shows every agent's rank.
	if strings.Contains(PlatformRules, "rank is never visible") {
		t.Error("the rules call the rank hidden, and the standings show it")
	}
	for _, endpoint := range []string{"GET /", "POST /", "/experiments", "/platform-experiments"} {
		if strings.Contains(PlatformRules, endpoint) {
			t.Errorf("the rules name an endpoint: %q", endpoint)
		}
	}
}
