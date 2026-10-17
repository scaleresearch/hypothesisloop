package httpapi

import (
	"context"

	"github.com/danielgtaylor/huma/v2"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/platformexperiments"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
)

type modelUsageAPI struct {
	experiments *platformexperiments.PlatformExperimentsService
	access      *auth.Access
	logger      *zap.Logger
}

// RegisterModelUsage registers the read of a platform experiment's model usage on doc.
func RegisterModelUsage(doc *apidocs.Doc, experiments *platformexperiments.PlatformExperimentsService, logger *zap.Logger) {
	a := modelUsageAPI{experiments: experiments, access: doc.Access, logger: logger}
	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "get-platform-experiment-model-usage", Method: "GET", Path: "/platform-experiments/{id}/model-usage",
		Summary: "Get the model tokens the platform's agents spent, per agent and model", Tags: []string{"platform-experiments"},
		Description: "The model calls the platform made for this platform experiment's hosted agents and coordinator, " +
			"summed per agent and model as the provider counted them: input, output, cache write and cache read tokens.",
	}, a.get)
}

func (a modelUsageAPI) get(ctx context.Context, in *struct {
	ID string `path:"id"`
}) (*struct {
	Body *platformexperiments.ModelUsage
}, error) {
	if err := a.access.PlatformExperiment(ctx, in.ID); err != nil {
		return nil, err
	}
	usage, err := a.experiments.ModelUsage(ctx, in.ID)
	if err != nil {
		return nil, auth.Internal(a.logger, "get platform experiment model usage", err, zap.String("id", in.ID))
	}
	return &struct {
		Body *platformexperiments.ModelUsage
	}{Body: usage}, nil
}
