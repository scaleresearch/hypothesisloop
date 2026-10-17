package platformexperiments

import (
	"context"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// SessionStore keeps one opaque snapshot per agent and per coordinator. *objectstore.Client is
// the one in use.
type SessionStore interface {
	PutSession(ctx context.Context, platformExperimentID, agentID string, snapshot []byte) error
	GetSession(ctx context.Context, platformExperimentID, agentID string, maxBytes int64) ([]byte, error)
}

// Sessions keeps the model session of each sandboxed agent and coordinator, so that it outlives the
// machine they run on. An empty agentID is the coordinator's.
type Sessions struct {
	store SessionStore
}

func NewSessions(store SessionStore) *Sessions {
	return &Sessions{store: store}
}

func (s *Sessions) Put(ctx context.Context, platformExperimentID, agentID string, snapshot []byte) error {
	return s.store.PutSession(ctx, platformExperimentID, agentID, snapshot)
}

// Get is the stored session, or domain.ErrNoSession.
func (s *Sessions) Get(ctx context.Context, platformExperimentID, agentID string) ([]byte, error) {
	return s.store.GetSession(ctx, platformExperimentID, agentID, domain.MaxAgentSessionBytes)
}
