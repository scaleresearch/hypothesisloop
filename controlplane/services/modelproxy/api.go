package modelproxy

import (
	"context"

	"github.com/danielgtaylor/huma/v2"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
)

// UsageReader sums the model calls made for a platform experiment.
type UsageReader interface {
	ModelUsageOf(ctx context.Context, platformExperimentID string) ([]metricsdb.AgentModelUsage, error)
}

func (m MetricsDB) ModelUsageOf(ctx context.Context, platformExperimentID string) ([]metricsdb.AgentModelUsage, error) {
	return metricsdb.ModelUsageOf(ctx, m.URL, platformExperimentID)
}

// ModelUsage is the model calls the platform made for one platform experiment's agents.
type ModelUsage struct {
	PlatformExperimentID string `json:"platform_experiment_id"`
	// Usage is one entry per agent and model, the coordinator's under agent_id coordinator.
	Usage []metricsdb.AgentModelUsage `json:"usage"`
}

// RegisterHuma registers the read of a platform experiment's model usage on doc.
func RegisterHuma(doc *apidocs.Doc, usage UsageReader, logger *zap.Logger) {
	access := doc.Access
	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "get-platform-experiment-model-usage", Method: "GET", Path: "/platform-experiments/{id}/model-usage",
		Summary: "Get the model tokens the platform's agents spent, per agent and model", Tags: []string{"platform-experiments"},
		Description: "The model calls the platform made for this platform experiment's hosted agents and coordinator, " +
			"summed per agent and model as the provider counted them: input, output, cache write and cache read tokens.",
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*struct{ Body *ModelUsage }, error) {
		if err := access.PlatformExperiment(ctx, in.ID); err != nil {
			return nil, err
		}
		calls, err := usage.ModelUsageOf(ctx, in.ID)
		if err != nil {
			return nil, auth.Internal(logger, "get platform experiment model usage", err, zap.String("id", in.ID))
		}
		return &struct{ Body *ModelUsage }{Body: &ModelUsage{PlatformExperimentID: in.ID, Usage: calls}}, nil
	})
}
