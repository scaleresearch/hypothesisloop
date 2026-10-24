package httpapi

import (
	"context"

	"github.com/danielgtaylor/huma/v2"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/models"
)

type modelsAPI struct {
	models *models.Service
	logger *zap.Logger
}

// registerModels registers the listing of the models the platform serves on doc.
func registerModels(doc *Doc, served *models.Service, logger *zap.Logger) {
	a := modelsAPI{models: served, logger: logger}
	register(doc, auth.AudienceAgent, huma.Operation{
		OperationID: "list-models", Method: "GET", Path: "/models", Tags: []string{"models"},
		Summary: "List the models agents and coordinators may run on",
		Description: "Every model the platform serves, each by its reference <provider>/<model id>, the one an agent type's settings.model, an added agent's model, an agents entry's model and a coordinator_model name: " +
			"the anthropic models of the platform's catalog, then the openai models of its catalog for its ChatGPT subscription, then the models OpenRouter serves that take tools at a price it states in advance, as OpenRouter lists them now. A provider the platform holds no credential for lists none. " +
			"Naming a model this does not list is refused with 422 model_not_served. An agent runs on its own model, else its agent type's, else, for a researcher, its platform experiment's coordinator_model, else default_model for an executor and coordinator_model for a researcher; a sandboxed coordinator runs on its platform experiment's coordinator_model, else its agent type's, else coordinator_model.",
	}, a.list)
}

func (a modelsAPI) list(ctx context.Context, _ *struct{}) (*struct{ Body *models.Listing }, error) {
	listing, err := a.models.List(ctx)
	if err != nil {
		return nil, auth.Internal(a.logger, "list models", err)
	}
	return &struct{ Body *models.Listing }{Body: listing}, nil
}
