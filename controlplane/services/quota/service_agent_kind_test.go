package quota

import (
	"context"
	"testing"

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

func (f *fakeAgentStore) ListAgents(ctx context.Context, limit, offset int) ([]*domain.Agent, error) {
	return f.created, nil
}

func (f *fakeAgentStore) CountAgents(ctx context.Context) (int, error) {
	return len(f.created), nil
}

type noopProvisioner struct{}

func (noopProvisioner) ProvisionAgent(ctx context.Context, agentID string) error { return nil }

// TestRegisterAgent_KindDefaultsToAgent covers the backward-compat path: every existing caller
// never sends a kind, and must keep getting an AgentKindAgent row exactly as before this field
// existed.
func TestRegisterAgent_KindDefaultsToAgent(t *testing.T) {
	store := &fakeAgentStore{}
	svc := NewService(store, noopProvisioner{}, zap.NewNop())

	agent, err := svc.RegisterAgent(context.Background(), "agent-1", "Agent One", "")
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
	svc := NewService(store, noopProvisioner{}, zap.NewNop())

	agent, err := svc.RegisterAgent(context.Background(), "human-1", "Ada", domain.AgentKindHuman)
	if err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	if agent.Kind != domain.AgentKindHuman {
		t.Errorf("Kind = %q, want %q", agent.Kind, domain.AgentKindHuman)
	}
}
