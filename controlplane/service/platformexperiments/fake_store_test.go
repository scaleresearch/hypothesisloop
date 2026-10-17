package platformexperiments

import (
	"context"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain/agenttypes/agenttypestest"
)

// fakeStore is what the stores of these tests are made from: the built-in agent types are the
// catalog of every platform experiment and project, and every other method is left to whoever
// embeds it.
type fakeStore struct{ Store }

func (fakeStore) AgentTypeCatalog(context.Context, string) (domain.AgentTypeCatalog, error) {
	return agenttypestest.Catalog(), nil
}

func (fakeStore) ProjectAgentTypeCatalog(context.Context, string) (domain.AgentTypeCatalog, error) {
	return agenttypestest.Catalog(), nil
}

func (fakeStore) OnCoresOnly(context.Context, []string) (map[string]bool, error) {
	return map[string]bool{}, nil
}
