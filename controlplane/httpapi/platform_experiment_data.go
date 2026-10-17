package httpapi

import (
	"context"

	"github.com/danielgtaylor/huma/v2"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/platformexperiments"
)

type experimentDataAPI struct {
	experiments *platformexperiments.Service
	access      *auth.Access
	logger      *zap.Logger
}

// registerExperimentData registers what a platform experiment's durable data holds.
func registerExperimentData(doc *Doc, experiments *platformexperiments.Service, logger *zap.Logger) {
	a := experimentDataAPI{experiments: experiments, access: doc.Access, logger: logger}
	register(doc, auth.AudienceAgent, huma.Operation{
		OperationID: "get-platform-experiment-data-usage", Method: "GET", Path: "/platform-experiments/{id}/data-usage",
		Summary: "Get durable-data bytes held, per agent", Tags: []string{"platform-experiments"},
		Description: "Checkpoint and dataset bytes stored under this platform experiment, totalled and broken down by " +
			"agent, read live from the object store. max_bytes_per_agent is the ceiling admission refuses a submission " +
			"against (reason data_quota_exceeded).",
	}, a.usage)
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
