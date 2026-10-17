package httpapi

import (
	"context"
	"errors"

	"github.com/danielgtaylor/huma/v2"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/platformexperiments"
)

type experimentDataAPI struct {
	experiments *platformexperiments.Service
	access      *auth.Access
	logger      *zap.Logger
}

// registerExperimentData registers what a platform experiment's durable data holds and each
// agent's own key to it.
func registerExperimentData(doc *Doc, experiments *platformexperiments.Service, logger *zap.Logger) {
	a := experimentDataAPI{experiments: experiments, access: doc.Access, logger: logger}
	register(doc, auth.AudienceAgent, huma.Operation{
		OperationID: "get-platform-experiment-data-usage", Method: "GET", Path: "/platform-experiments/{id}/data-usage",
		Summary: "Get durable-data bytes held, per agent", Tags: []string{"platform-experiments"},
		Description: "Checkpoint and dataset bytes stored under this platform experiment, totalled and broken down by " +
			"agent, read live from the object store. max_bytes_per_agent is the ceiling admission refuses a submission " +
			"against (reason data_quota_exceeded).",
	}, a.usage)
	register(doc, auth.AudienceAgent, huma.Operation{
		OperationID: "get-agent-data-key", Method: "POST", Path: "/platform-experiments/{id}/data-key",
		Summary: "Get an agent's own key to the platform experiment's data", Tags: []string{"platform-experiments"},
		Description: "S3 credentials for a signed-up agent: read everything under shared, write under uri, the prefix " +
			"its jobs write below. The same key on every call, with no expiry. Refused once the platform experiment " +
			"is closed. `hl data-key` prints it as environment variables.",
	}, a.key)
}

func (a experimentDataAPI) usage(ctx context.Context, in *struct {
	ID string `path:"id"`
}) (*struct {
	Body *platformexperiments.DataUsage
}, error) {
	if err := a.access.PlatformExperiment(ctx, in.ID); err != nil {
		return nil, err
	}
	usage, err := a.experiments.DataUsage(ctx, in.ID)
	if err != nil {
		return nil, auth.Internal(a.logger, "get platform experiment data usage", err, zap.String("id", in.ID))
	}
	return &struct {
		Body *platformexperiments.DataUsage
	}{Body: usage}, nil
}

type dataKeyRequest struct {
	AgentID string `json:"agent_id"`
}

func (a experimentDataAPI) key(ctx context.Context, in *struct {
	ID   string `path:"id"`
	Body dataKeyRequest
}) (*struct{ Body *domain.DataAccess }, error) {
	if in.Body.AgentID == "" {
		return nil, huma.Error400BadRequest("agent_id is required")
	}
	if err := a.access.PlatformExperiment(ctx, in.ID); err != nil {
		return nil, err
	}
	if err := a.access.ActAs(ctx, in.Body.AgentID); err != nil {
		return nil, err
	}
	if err := a.experiments.RequireActive(ctx, in.ID, in.Body.AgentID); err != nil {
		return nil, err
	}
	dataAccess, err := a.experiments.AgentDataAccess(ctx, in.ID, in.Body.AgentID)
	switch {
	case errors.Is(err, domain.ErrNotSignedUp):
		return nil, huma.Error404NotFound(err.Error())
	case errors.Is(err, domain.ErrPlatformExperimentClosed):
		return nil, huma.Error409Conflict(err.Error())
	case err != nil:
		return nil, auth.Internal(a.logger, "get agent data key", err, zap.String("id", in.ID))
	}
	return &struct{ Body *domain.DataAccess }{Body: dataAccess}, nil
}
