package quota

import (
	"context"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"go.uber.org/zap"
)

// fakeAgentStore is a minimal in-memory Store, just enough to exercise RegisterAgent's kind
// handling without a real database.
type fakeAgentStore struct {
	created []*domain.Agent
}

func (f *fakeAgentStore) CreateAgent(ctx context.Context, agent *domain.Agent) error {
	f.created = append(f.created, agent)
	return nil
}

func (f *fakeAgentStore) ListAgents(ctx context.Context, filter db.AgentsFilter) ([]*domain.Agent, error) {
	return f.created, nil
}

func (f *fakeAgentStore) AgentStats(ctx context.Context, filter db.AgentsFilter) (*domain.AgentStats, error) {
	return &domain.AgentStats{Total: len(f.created)}, nil
}

// TestRegisterAgent_KindDefaultsToAgent covers the backward-compat path: every existing caller
// never sends a kind, and must keep getting an AgentKindAgent row exactly as before this field
// existed.
func TestRegisterAgent_KindDefaultsToAgent(t *testing.T) {
	store := &fakeAgentStore{}
	svc := NewService(store, zap.NewNop())

	agent, err := svc.RegisterAgent(context.Background(), "agent-1", "Agent One", "default", "")
	if err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	if agent.Kind != domain.AgentKindAgent {
		t.Errorf("Kind = %q, want %q", agent.Kind, domain.AgentKindAgent)
	}
	if store.created[0].Kind != domain.AgentKindAgent {
		t.Errorf("persisted Kind = %q, want %q", store.created[0].Kind, domain.AgentKindAgent)
	}
}

// TestRegisterAgent_KindHuman covers the new path: a real person registering as a participant.
func TestRegisterAgent_KindHuman(t *testing.T) {
	store := &fakeAgentStore{}
	svc := NewService(store, zap.NewNop())

	agent, err := svc.RegisterAgent(context.Background(), "human-1", "Ada", "default", domain.AgentKindHuman)
	if err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	if agent.Kind != domain.AgentKindHuman {
		t.Errorf("Kind = %q, want %q", agent.Kind, domain.AgentKindHuman)
	}
}
