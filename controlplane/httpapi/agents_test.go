package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/accounts"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
)

// takenAgents is an agent store in which every id in it is taken.
type takenAgents struct {
	accounts.AgentStore
	taken map[string]bool
}

func (s takenAgents) CreateAgent(_ context.Context, agent *domain.Agent) error {
	if s.taken[agent.ID] {
		return fmt.Errorf("%w: %s", db.ErrAgentExists, agent.ID)
	}
	return nil
}

// A taken id was told from the database's error text, and an agent registered without a name was
// silently named after its id.
func TestRegisteringAnAgentRefusesATakenIDAndRequiresAName(t *testing.T) {
	r := authtest.Router(authtest.As(auth.Principal{Kind: auth.KindUser, UserID: "u-1", OrgIDs: []string{"org-a"}}))
	doc := apidocs.New(r, "test", "1.0.0", "", auth.NewAccess(authtest.Placement{}), zap.NewNop())
	RegisterAgents(doc, accounts.NewAgents(takenAgents{taken: map[string]bool{"veteran": true}}, zap.NewNop()))

	if code, out := call(t, r, "POST", "/agents", `{"id":"veteran","name":"Veteran"}`); code != http.StatusConflict || out["reason"] != "agent_exists" {
		t.Errorf("a taken id: got = %d %v, want 409 agent_exists", code, out)
	}
	if code, out := call(t, r, "POST", "/agents", `{"id":"rookie","name":"Rookie"}`); code != http.StatusCreated || out["name"] != "Rookie" {
		t.Errorf("a free id: got = %d %v, want 201 with its name", code, out)
	}
	if code, out := call(t, r, "POST", "/agents", `{"id":"nameless"}`); code != http.StatusUnprocessableEntity {
		t.Errorf("no name: got = %d %v, want 422", code, out)
	}
}

// pagedAgents records the limit each list was read with.
type pagedAgents struct {
	accounts.AgentStore
	limits *[]int
}

func (s pagedAgents) ListAgents(_ context.Context, filter db.AgentsFilter) ([]*domain.Agent, error) {
	*s.limits = append(*s.limits, filter.Limit)
	return nil, nil
}

func (pagedAgents) AgentStats(context.Context, db.AgentsFilter) (*domain.AgentStats, error) {
	return &domain.AgentStats{}, nil
}

// The list shortened a limit over the maximum to it, so a short page read as the whole set.
func TestTheAgentListRefusesAPageOverTheMaximum(t *testing.T) {
	var read []int
	r := authtest.Router(authtest.Staff)
	doc := apidocs.New(r, "test", "1.0.0", "", auth.NewAccess(authtest.Placement{}), zap.NewNop())
	RegisterAgents(doc, accounts.NewAgents(pagedAgents{limits: &read}, zap.NewNop()))

	for _, query := range []string{"limit=201", "limit=100000", "limit=-1", "offset=-1"} {
		if code, out := call(t, r, "GET", "/agents?"+query, ""); code != http.StatusUnprocessableEntity || out["reason"] != "invalid_page" {
			t.Errorf("GET /agents?%s: got = %d %v, want 422 invalid_page", query, code, out)
		}
	}
	if len(read) != 0 {
		t.Fatalf("a refused page was read from the store with limits %v", read)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, authtest.Authorize(httptest.NewRequest(http.MethodGet, "/agents?limit=200", nil)))
	if rec.Code != http.StatusOK || len(read) != 1 || read[0] != 200 {
		t.Errorf("GET /agents?limit=200: got = %d, read with %v, want 200 and the store asked for 200: %s", rec.Code, read, rec.Body)
	}
}
