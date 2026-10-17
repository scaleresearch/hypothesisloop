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

type budgetAPI struct {
	platformExperimentsAPI
}

// registerPlatformExperimentBudget registers the reads of a platform experiment's health, quotas,
// stages and capacity, and its summary.
func registerPlatformExperimentBudget(doc *Doc, experiments *platformexperiments.Service, logger *zap.Logger) {
	a := budgetAPI{platformExperimentsAPI{experiments: experiments, access: doc.Access, logger: logger}}

	register(doc, auth.AudienceAgent, huma.Operation{
		OperationID: "get-platform-experiment-health", Method: "GET", Path: "/platform-experiments/{id}/health",
		Summary: "Where each agent of a run last stood", Tags: []string{"platform-experiments"},
		Description: "For each agent and the sandboxed coordinator, the latest values of the run's health series, read from the metrics store, which records them while the run is open or running: " +
			"seconds to its first job and its first completed job, jobs in flight, restarts of its machine over the whole run and messages the coordinator sent it. " +
			"agents is empty for a run that never opened.",
	}, a.getPlatformExperimentHealth)

	register(doc, auth.AudienceCoordinator, afterClose(huma.Operation{
		OperationID: "set-platform-experiment-summary", Method: "PUT", Path: "/platform-experiments/{id}/summary",
		Summary: "Record the operator's narrative verdict on a run", Tags: []string{"platform-experiments"},
		Description: "Replaces the summary and answers the platform experiment with it. Writable after the experiment closes: it is the one field whose whole point " +
			"is to be written once the run is over and the results are in. A person or an API key writes it at any time. " +
			"A coordinator's token writes it until five minutes after the close (domain.CoordinatorWindDown), when the token ends (401). 422 too_long past 32768 bytes.",
	}), a.setPlatformExperimentSummary)

	register(doc, auth.AudienceAgent, huma.Operation{
		OperationID: "list-platform-experiment-quotas", Method: "GET", Path: "/platform-experiments/{id}/quotas",
		Summary: "List per-agent quotas for a platform experiment", Tags: []string{"platform-experiments"},
		Description: "Each signed-up agent's share of the budget, what it has used and what a job submitted now may still reserve. Guaranteed and burst jobs spend the same share: burst gives no hours on top of it.",
	}, a.listPlatformExperimentQuotas)

	register(doc, auth.AudienceAgent, huma.Operation{
		OperationID: "get-agent-quota", Method: "GET", Path: "/platform-experiments/{id}/quotas/{agent_id}",
		Summary: "Get one agent's quota within a platform experiment", Tags: []string{"platform-experiments"},
		Description: "One row of what GET /platform-experiments/{id}/quotas returns for every agent, and with it data_bytes and max_data_bytes: " +
			"what the agent holds in the object store and the ceiling a submit is refused on with data_quota_exceeded, where the platform sets one. " +
			"503 object_store_unavailable while the object store does not answer. " +
			"404 planner_has_no_quota for a draft's planner, which submits no job and needs none: the message says where the run's budget comes from instead. " +
			"404 quota_not_granted while the platform experiment is open and the agent holds none yet: quota is granted at the start, and the message is the platform experiment's status_reason, what the caller does next.",
	}, a.getAgentQuota)

	register(doc, auth.AudienceAgent, huma.Operation{
		OperationID: "get-stages", Method: "GET", Path: "/platform-experiments/{id}/stages",
		Summary: "Get the stage ladder and current standing of a platform experiment", Tags: []string{"platform-experiments"},
		Description: "The ladder, which stage is running, progress through it, and which signed-up agents are still active vs cut. Open, the ladder is the one declared. From the start it is the one the run has: a single stage where fewer than 5 researchers joined, which has no boundary and sends no stage.boundary. Each stage carries max_job_hours: the longest one job may run while it is current (0/absent = unlimited). Where the next cut falls is not shown; every agent's best value is, at GET /platform-experiments/{id}/results.",
	}, a.getStages)

	// ---- resource catalog ----

	register(doc, auth.AudienceAgent, huma.Operation{
		OperationID: "get-resource-capacity", Method: "GET", Path: "/resource-catalog/capacity",
		Summary: "Get live per-cluster accelerator capacity", Tags: []string{"resource-catalog"},
		Description: "Live {accelerator_type, available, total} per cluster the jobs of one org can be placed on: the org's own " +
			"clusters and the ones the platform shares, without the ones the org turned off for its jobs. The org is the one your token is bound to: every API key, agent and " +
			"coordinator token has exactly one, and so does a person in one org. A person in several orgs names it with org_id, " +
			"and is answered 422 naming org_id without it. An org you do not reach is not found. Staff read one org with org_id and every " +
			"cluster that reports without it. accelerator_type is the exact " +
			"driver-published string a job spec takes, so what you read here is what you submit. pod_resources on a type " +
			"is what the platform adds to every node of a job on it, which the job does not state itself. A job's cpu, memory " +
			"and accelerator_count are per node, so per_node on a cluster says the most cores and memory one node has, installed " +
			"and free now, and per_node_available on a type the most of it free on one node now. Check before " +
			"choosing: a type with no live capacity anywhere QUEUES FOREVER and never errors. Only " +
			"currently-schedulable nodes count — powered-off hardware never appears. " +
			"platform_compute_limit is how much of the platform's shared clusters the org's placed jobs may reserve, and each of its projects', in accelerator hours per hour " +
			"(acch_per_hour of the type times accelerator_count), with what they reserve now: a job that would pass either waits queued as org_compute_limit or project_compute_limit, " +
			"and one that alone passes the project's where no cluster of the org's own can hold it is refused 422 exceeds_project_compute_limit."}, a.getResourceCapacity)
}

func (a budgetAPI) getPlatformExperimentHealth(ctx context.Context, in *struct {
	ID string `path:"id"`
}) (*struct {
	Body *platformexperiments.RunHealth
}, error) {
	if err := a.access.PlatformExperiment(ctx, in.ID); err != nil {
		return nil, err
	}
	health, err := a.experiments.Health(ctx, in.ID)
	if errors.Is(err, platformexperiments.ErrPlatformExperimentNotFound) {
		return nil, huma.Error404NotFound(err.Error())
	}
	if err != nil {
		return nil, auth.Internal(a.logger, "get platform experiment health", err, zap.String("id", in.ID))
	}
	return &struct {
		Body *platformexperiments.RunHealth
	}{Body: health}, nil
}

type runSummaryRequest struct {
	Summary string `json:"summary"`
}

func (a budgetAPI) setPlatformExperimentSummary(ctx context.Context, in *struct {
	ID   string `path:"id"`
	Body runSummaryRequest
}) (*struct{ Body *domain.PlatformExperiment }, error) {
	if err := a.access.PlatformExperiment(ctx, in.ID); err != nil {
		return nil, err
	}
	pe, err := a.experiments.SetSummary(ctx, in.ID, in.Body.Summary)
	if err != nil {
		return nil, refused(a.logger, "set platform experiment summary", err)
	}
	return a.answer(ctx, pe)
}

func (a budgetAPI) listPlatformExperimentQuotas(ctx context.Context, in *struct {
	ID string `path:"id"`
}) (*struct {
	Body []*platformexperiments.AgentQuotaView
}, error) {
	if err := a.access.PlatformExperiment(ctx, in.ID); err != nil {
		return nil, err
	}
	quotas, err := a.experiments.ListQuotas(ctx, in.ID)
	if errors.Is(err, platformexperiments.ErrPlatformExperimentNotFound) {
		return nil, huma.Error404NotFound(err.Error())
	}
	if err != nil {
		return nil, auth.Internal(a.logger, "list platform experiment quotas", err, zap.String("id", in.ID))
	}
	return &struct {
		Body []*platformexperiments.AgentQuotaView
	}{Body: quotas}, nil
}

func (a budgetAPI) getAgentQuota(ctx context.Context, in *struct {
	ID      string `path:"id"`
	AgentID string `path:"agent_id"`
}) (*struct {
	Body *platformexperiments.AgentQuotaView
}, error) {
	if err := a.access.PlatformExperiment(ctx, in.ID); err != nil {
		return nil, err
	}
	reader, err := caller(ctx, a.logger)
	if err != nil {
		return nil, err
	}
	aq, err := a.experiments.AgentQuota(ctx, in.AgentID, in.ID, reader)
	if errors.Is(err, platformexperiments.ErrNoQuota) {
		return nil, huma.Error404NotFound("agent " + in.AgentID + " has no quota in platform experiment " + in.ID)
	}
	if err != nil {
		return nil, err
	}
	return &struct {
		Body *platformexperiments.AgentQuotaView
	}{Body: aq}, nil
}

func (a budgetAPI) getStages(ctx context.Context, in *struct {
	ID string `path:"id"`
}) (*struct {
	Body platformexperiments.StagesStatus
}, error) {
	if err := a.access.PlatformExperiment(ctx, in.ID); err != nil {
		return nil, err
	}
	stages, err := a.experiments.Stages(ctx, in.ID)
	if errors.Is(err, platformexperiments.ErrPlatformExperimentNotFound) {
		return nil, huma.Error404NotFound(err.Error())
	}
	if err != nil {
		return nil, err
	}
	return &struct {
		Body platformexperiments.StagesStatus
	}{Body: *stages}, nil
}

type liveCapacity struct {
	Clusters             []platformexperiments.ClusterCapacity      `json:"clusters"`
	PlatformComputeLimit *platformexperiments.PlatformComputeLimit `json:"platform_compute_limit,omitempty" doc:"The org's limits on the platform's shared clusters, in accelerator hours per hour, and what its placed jobs reserve of them. Absent from staff's read of every cluster."`
}

func (a budgetAPI) getResourceCapacity(ctx context.Context, in *struct {
	OrgID string `query:"org_id" doc:"The org whose clusters to read. Required only when your token reaches more than one org."`
}) (*struct{ Body liveCapacity }, error) {
	orgID, every, err := a.access.PlacementOrg(ctx, in.OrgID)
	if err != nil {
		return nil, err
	}
	clusters, err := a.experiments.ResourceCapacity(ctx, orgID, every)
	if err != nil {
		return nil, auth.Internal(a.logger, "get resource capacity", err)
	}
	answer := liveCapacity{Clusters: clusters}
	if !every {
		principal, _ := auth.FromContext(ctx)
		if answer.PlatformComputeLimit, err = a.experiments.ComputeLimit(ctx, orgID, principal.ProjectID); err != nil {
			return nil, auth.Internal(a.logger, "get platform compute limit", err)
		}
	}
	return &struct{ Body liveCapacity }{Body: answer}, nil
}
