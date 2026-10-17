package platformexperiments

import (
	"context"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain/agenttypes"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
)

// fakeStore is what the stores of these tests are made from: the built-in agent types are the
// catalog of every platform experiment and project, and every other method is left to whoever
// embeds it.
type fakeStore struct{ PlatformExperimentsStore }

func (fakeStore) AgentTypeCatalog(context.Context, string) (domain.AgentTypeCatalog, error) {
	return agenttypes.Catalog(), nil
}

func (fakeStore) ProjectAgentTypeCatalog(context.Context, string) (domain.AgentTypeCatalog, error) {
	return agenttypes.Catalog(), nil
}

// registerHuma registers the platform experiment operations this package still holds.
func registerHuma(doc *apidocs.Doc, peh *PlatformExperimentsHandler) {
	RegisterHuma(doc, peh)
	RegisterBudgetHuma(doc, peh)
}
