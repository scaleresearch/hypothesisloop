package httpapi

import (
	"context"
	"errors"

	"github.com/danielgtaylor/huma/v2"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/platformexperiments"
)

type situationAPI struct {
	experiments *platformexperiments.Service
	access      *auth.Access
	logger      *zap.Logger
}

// registerSituation registers the situation read.
func registerSituation(doc *Doc, experiments *platformexperiments.Service, logger *zap.Logger) {
	a := situationAPI{experiments: experiments, access: doc.Access, logger: logger}
	register(doc, auth.AudienceAgent, huma.Operation{
		OperationID: "get-platform-experiment-situation", Method: "GET", Path: "/platform-experiments/{id}/situation",
		Summary: "Where the run stands now, in one read", Tags: []string{"platform-experiments"},
		Description: "Computed on every read: the run's clock (ends_at, minutes_left, the stage and when the next starts; on a closed run minutes_left 0 with closed_at and close_reason, on one not started how long it will run once it starts), " +
			"the objective on each ranking metric (the declared baseline's value, the best a researcher's completed job holding every constraint reached, by which job and agent, " +
			"the target its target_vs_baseline sets against the baseline's value and the gap to it, and the best an hour and three hours ago), " +
			"the capacity the run's jobs could use (free and total of its baseline job's type on each cluster its jobs can be placed on, fits_now: how many jobs shaped like the baseline job place now, node by node, and the run's jobs queued and running), " +
			"the run's budget and spend, for a coordinator, a person or a key each hypothesis with jobs queued or running or checked out within two hours and who is on it, and for each agent its jobs queued and running, the subagents and turn its harness last reported, its activity, minutes without a job in flight, " +
			"the messages the reader may still send it this hour, its model account's allowance, and its share and the room left in it, " +
			"and ideas_from_people: how many open ideas people registered, those no agent has settled or built on yet, and the command that lists them. " +
			"An agent's token reads the run and its own row. A coordinator, a person or a key reads every row and the hosted coordinator: when its summary was last written, how many jobs completed since, and its model allowance.",
	}, a.get)

}

func (a situationAPI) get(ctx context.Context, in *struct {
	ID string `path:"id"`
}) (*struct {
	Body *platformexperiments.Situation
}, error) {
	if err := a.access.PlatformExperiment(ctx, in.ID); err != nil {
		return nil, err
	}
	caller, err := caller(ctx, a.logger)
	if err != nil {
		return nil, err
	}
	agentID := ""
	if caller.Kind == auth.KindAgent {
		agentID = caller.AgentID
	}
	situation, err := a.experiments.Situation(ctx, in.ID, agentID, caller.Actor())
	if errors.Is(err, platformexperiments.ErrPlatformExperimentNotFound) {
		return nil, huma.Error404NotFound(err.Error())
	}
	if err != nil {
		return nil, auth.Internal(a.logger, "get platform experiment situation", err, zap.String("id", in.ID))
	}
	return &struct {
		Body *platformexperiments.Situation
	}{Body: situation}, nil
}
