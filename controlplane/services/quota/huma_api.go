package quota

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// reasonError preserves the historical {"reason","message"} error envelope used
// by signup (a client-facing, non-fault rejection) instead of the {"error":...}
// envelope, matching the pre-Huma wire shape.
type reasonError struct {
	status  int
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

func (e *reasonError) Error() string  { return e.Message }
func (e *reasonError) GetStatus() int { return e.status }

// RegisterHuma registers every quota-service operation (agents,
// platform-experiments, donations, resource catalog) on the shared Huma doc.
func RegisterHuma(doc *apidocs.Doc, h *Handler, peh *PlatformExperimentsHandler) {
	access := doc.Access

	// ---- agents ----

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "register-agent", Method: "POST", Path: "/agents",
		Summary: "Register an agent", Tags: []string{"agents"},
		DefaultStatus: 201,
		Description:   "Registers a lasting identity under an id of your choosing: a person (kind=\"human\"), or an agent whose standing should carry from one platform experiment to the next. Returns 409 if the id already exists. It joins nothing: POST /platform-experiments/{id}/agents with agent_id does, and that call alone creates agents that need no identity of their own.",
	}, func(ctx context.Context, in *struct {
		Body struct {
			ID    string `json:"id" doc:"Stable unique agent id"`
			Name  string `json:"name,omitempty" required:"false" doc:"Human-readable display name; defaults to id"`
			Kind  string `json:"kind,omitempty" required:"false" doc:"\"agent\" (default) or \"human\""`
			OrgID string `json:"org_id,omitempty" required:"false" doc:"The org the agent belongs to. Needed only from a token that is not bound to a single org."`
		}
	}) (*struct{ Body *domain.Agent }, error) {
		// Validated here rather than only where it is used: an agent id is pasted verbatim into
		// Kubernetes object names and into the object-store key policy that decides what a
		// job may write. Registration is the one moment it can be refused for free.
		if err := domain.ValidateIdentifier("id", in.Body.ID); err != nil {
			return nil, huma.Error400BadRequest(err.Error())
		}
		if !domain.ValidAgentKind(domain.AgentKind(in.Body.Kind)) {
			return nil, huma.Error400BadRequest("kind must be \"agent\" or \"human\", got " + in.Body.Kind)
		}
		// id is the only identity that matters; name is presentation-only (UI/leaderboard), so
		// requiring it would reject an otherwise complete registration over a cosmetic field.
		name := in.Body.Name
		if name == "" {
			name = in.Body.ID
		}
		orgID, err := access.Org(ctx, in.Body.OrgID)
		if err != nil {
			return nil, err
		}
		agent, err := h.svc.RegisterAgent(ctx, in.Body.ID, name, orgID, domain.AgentKind(in.Body.Kind))
		if err != nil {
			if strings.Contains(err.Error(), "duplicate key") || strings.Contains(err.Error(), "unique constraint") {
				return nil, huma.Error409Conflict("agent already exists: " + in.Body.ID)
			}
			h.logger.Error("registerAgent failed")
			return nil, huma.Error500InternalServerError("failed to register agent: " + err.Error())
		}
		return &struct{ Body *domain.Agent }{Body: agent}, nil
	})

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "list-agents", Method: "GET", Path: "/agents",
		Summary: "List registered agents", Tags: []string{"agents"},
		Description: "Filter with ?platform_experiment_id (the agents signed up to it) and ?q (search id/name). " +
			"Optional ?limit (default 20, max 200) and ?offset, newest first. Total match count is " +
			"returned in the X-Total-Count response header.",
	}, func(ctx context.Context, in *struct {
		PlatformExperimentID string `query:"platform_experiment_id" doc:"Only the agents signed up to this platform experiment."`
		Search               string `query:"q" doc:"Case-insensitive substring match against id and name."`
		ProjectID            string `query:"project_id" doc:"Only the agents signed up to a platform experiment of this project."`
		Limit                int    `query:"limit" doc:"Page size, default 20, maximum 200. The default is small because the whole response is meant to be read by an agent: ask for more deliberately, using X-Total-Count to decide."`
		Offset               int    `query:"offset" doc:"Rows to skip, for paging through X-Total-Count total matches."`
	}) (*struct {
		Body       []*domain.Agent
		TotalCount int `header:"X-Total-Count"`
	}, error) {
		if in.Limit < 0 || in.Offset < 0 {
			return nil, huma.Error400BadRequest("limit and offset must not be negative")
		}
		scope, err := access.ListScope(ctx, in.ProjectID)
		if err != nil {
			return nil, err
		}
		filter := db.AgentsFilter{PlatformExperimentID: in.PlatformExperimentID, Search: in.Search, Scope: scope, Limit: in.Limit, Offset: in.Offset}
		agents, err := h.svc.store.ListAgents(ctx, filter)
		if err != nil {
			return nil, huma.Error500InternalServerError("failed to list agents")
		}
		stats, err := h.svc.store.AgentStats(ctx, filter)
		if err != nil {
			return nil, huma.Error500InternalServerError("failed to count agents")
		}
		return &struct {
			Body       []*domain.Agent
			TotalCount int `header:"X-Total-Count"`
		}{Body: agents, TotalCount: stats.Total}, nil
	})

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "agent-stats", Method: "GET", Path: "/agents/stats",
		Summary: "Count registered agents, those with a top-3 placement, and their mean performance score", Tags: []string{"agents"},
		Description: "Whole-set figures for the same ?platform_experiment_id and ?q filters GET /agents accepts, " +
			"aggregated in PostgreSQL. A list read is one page, so these cannot be tallied from it.",
	}, func(ctx context.Context, in *struct {
		PlatformExperimentID string `query:"platform_experiment_id" doc:"Only the agents signed up to this platform experiment."`
		Search               string `query:"q" doc:"Case-insensitive substring match against id and name."`
		ProjectID            string `query:"project_id" doc:"Only the agents signed up to a platform experiment of this project."`
	}) (*struct{ Body *domain.AgentStats }, error) {
		scope, err := access.ListScope(ctx, in.ProjectID)
		if err != nil {
			return nil, err
		}
		stats, err := h.svc.store.AgentStats(ctx, db.AgentsFilter{PlatformExperimentID: in.PlatformExperimentID, Search: in.Search, Scope: scope})
		if err != nil {
			return nil, huma.Error500InternalServerError("failed to count agents")
		}
		return &struct{ Body *domain.AgentStats }{Body: stats}, nil
	})

	// ---- platform experiments ----

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "create-platform-experiment", Method: "POST", Path: "/platform-experiments",
		Summary: "Create a platform experiment", Tags: []string{"platform-experiments"},
		DefaultStatus: 201,
		Description: "Operator/dashboard endpoint. project_id, name, budget_accelerator_hours, max_agents, report_interval_seconds and ends_at are required. " +
			"default_quota_tier controls signups that omit quota_tier and defaults to \"guaranteed\".",
	}, func(ctx context.Context, in *struct {
		Body CreatePlatformExperimentRequest
	}) (*struct{ Body *domain.PlatformExperiment }, error) {
		if in.Body.Name == "" || in.Body.BudgetAcceleratorHours <= 0 {
			return nil, huma.Error400BadRequest("name and budget_accelerator_hours are required")
		}
		if in.Body.ProjectID == "" {
			return nil, huma.Error400BadRequest("project_id is required")
		}
		if err := access.Project(ctx, in.Body.ProjectID); err != nil {
			return nil, err
		}
		pe, err := peh.svc.Create(ctx, in.Body)
		if errors.Is(err, ErrInvalidPlatformExperiment) {
			return nil, huma.Error400BadRequest(err.Error())
		}
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		return &struct{ Body *domain.PlatformExperiment }{Body: pe}, nil
	})

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "list-platform-experiments", Method: "GET", Path: "/platform-experiments",
		Summary: "List platform experiments", Tags: []string{"platform-experiments"},
		Description: "Filter with ?status, ?q (search name/description), ?limit, ?offset. " +
			"?sort selects order: created_at, starts_at, name, or status, optionally prefixed with " +
			"- for descending (default -created_at). Total match count is returned in the " +
			"X-Total-Count response header.",
	}, func(ctx context.Context, in *struct {
		Status    string `query:"status" doc:"draft, open, running or closed."`
		Search    string `query:"q" doc:"Case-insensitive substring match against name and description."`
		ProjectID string `query:"project_id" doc:"Only the platform experiments of this project."`
		Limit     int    `query:"limit" doc:"Page size, default 20, maximum 200. The default is small because the whole response is meant to be read by an agent: ask for more deliberately, using X-Total-Count to decide."`
		Offset    int    `query:"offset" doc:"Rows to skip, for paging through X-Total-Count total matches."`
		Sort      string `query:"sort" doc:"created_at, name or status, optionally prefixed with a minus for descending."`
	}) (*struct {
		Body       []*domain.PlatformExperiment
		TotalCount int `header:"X-Total-Count"`
	}, error) {
		if in.Status != "" && !domain.ValidPlatformExperimentStatus(domain.PlatformExperimentStatus(in.Status)) {
			return nil, huma.Error400BadRequest("unknown status " + in.Status)
		}
		if !domain.ValidSortField(in.Sort, db.PlatformExperimentSortFields) {
			return nil, huma.Error400BadRequest("unknown sort field " + in.Sort)
		}
		if in.Limit < 0 || in.Offset < 0 {
			return nil, huma.Error400BadRequest("limit and offset must not be negative")
		}
		scope, err := access.ListScope(ctx, in.ProjectID)
		if err != nil {
			return nil, err
		}
		filter := db.PlatformExperimentsFilter{
			Scope:  scope,
			Status: in.Status,
			Search: in.Search,
			Limit:  in.Limit,
			Offset: in.Offset,
			Sort:   in.Sort,
		}
		pes, err := peh.svc.store.ListPlatformExperiments(ctx, filter)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		total, err := peh.svc.store.CountPlatformExperiments(ctx, filter)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		if pes == nil {
			pes = []*domain.PlatformExperiment{}
		}
		return &struct {
			Body       []*domain.PlatformExperiment
			TotalCount int `header:"X-Total-Count"`
		}{Body: pes, TotalCount: total}, nil
	})

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "get-job-cost", Method: "GET", Path: "/experiments/{id}/cost",
		Summary: "Get what a job actually cost", Tags: []string{"platform-experiments"},
		Description: "Actual consumption, not the admission-time estimate: confirmed-alive hours and the " +
			"accelerator/CPU hours billed for them. `settled` distinguishes a final bill from a running total. " +
			"The estimate is returned alongside so the two can be compared.",
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*struct{ Body *JobCost }, error) {
		if err := access.Experiment(ctx, in.ID); err != nil {
			return nil, err
		}
		cost, err := peh.svc.JobCost(ctx, in.ID)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		if cost == nil {
			return nil, huma.Error404NotFound("experiment not found")
		}
		return &struct{ Body *JobCost }{Body: cost}, nil
	})

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "get-platform-experiment", Method: "GET", Path: "/platform-experiments/{id}",
		Summary: "Get a platform experiment", Tags: []string{"platform-experiments"},
		Description: "Returns status, budget, the stage ladder and the list of signed-up agents.",
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*struct{ Body *domain.PlatformExperiment }, error) {
		if err := access.PlatformExperiment(ctx, in.ID); err != nil {
			return nil, err
		}
		pe, err := peh.svc.store.GetPlatformExperiment(ctx, in.ID)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		if pe == nil {
			return nil, huma.Error404NotFound("platform experiment not found")
		}
		signups, err := peh.svc.store.ListSignups(ctx, in.ID)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		pe.SignedUpAgents = signups
		pe.SignupCount = len(signups)
		return &struct{ Body *domain.PlatformExperiment }{Body: pe}, nil
	})

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "update-platform-experiment", Method: "PUT", Path: "/platform-experiments/{id}",
		Summary: "Update a platform experiment", Tags: []string{"platform-experiments"},
		Description: "Amends an experiment while it is open or running. While open, every field including " +
			"`metrics` is editable — a mis-specified ranking metric can be fixed in place instead of closing " +
			"the experiment and discarding every agent's hypotheses and baseline work. Once running, only " +
			"`name` and `description` can be amended; budgets, `max_agents`, `metrics`, `report_interval_seconds` " +
			"and the schedule are locked because admission and stage decisions have already been made against " +
			"them. Rejected once the experiment is closed.",
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Body CreatePlatformExperimentRequest
	}) (*struct{ Body *domain.PlatformExperiment }, error) {
		if err := access.PlatformExperiment(ctx, in.ID); err != nil {
			return nil, err
		}
		pe, err := peh.svc.Update(ctx, in.ID, in.Body)
		if err != nil {
			return nil, huma.Error400BadRequest(err.Error())
		}
		return &struct{ Body *domain.PlatformExperiment }{Body: pe}, nil
	})

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "get-platform-experiment-signup", Method: "GET", Path: "/platform-experiments/{id}/signups/{agent_id}",
		Summary: "Read an agent's signup role", Tags: []string{"platform-experiments"},
		Description: "The role an agent was signed up under, which is what decides whether it is ranked and cut. " +
			"404 if that agent is not signed up to this platform experiment — not signed up is not a role.",
	}, func(ctx context.Context, in *struct {
		ID      string `path:"id"`
		AgentID string `path:"agent_id"`
	}) (*struct {
		Body struct {
			AgentID string `json:"agent_id"`
			Role    string `json:"role"`
		}
	}, error) {
		if err := access.PlatformExperiment(ctx, in.ID); err != nil {
			return nil, err
		}
		role, found, err := peh.svc.store.GetSignupRole(ctx, in.ID, in.AgentID)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		if !found {
			return nil, huma.Error404NotFound("agent is not signed up to this platform experiment")
		}
		out := &struct {
			Body struct {
				AgentID string `json:"agent_id"`
				Role    string `json:"role"`
			}
		}{}
		out.Body.AgentID = in.AgentID
		out.Body.Role = string(role)
		return out, nil
	})

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "start-platform-experiment", Method: "POST", Path: "/platform-experiments/{id}/start",
		Summary: "Start a platform experiment", Tags: []string{"platform-experiments"},
		Description: "Closes signup and allocates each signed-up agent its quota, so it can only " +
			"run once and only from status 'open'. Allocation divides the first stage's share of " +
			"the budget across the agents present at this moment -- an agent that has not signed " +
			"up by now gets nothing and cannot join later. Fails if nobody has signed up.",
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*struct {
		Body struct {
			Status string               `json:"status"`
			Quotas []*domain.AgentQuota `json:"quotas"`
		}
	}, error) {
		if err := access.PlatformExperiment(ctx, in.ID); err != nil {
			return nil, err
		}
		quotas, err := peh.svc.Start(ctx, in.ID)
		if err != nil {
			return nil, huma.Error400BadRequest(err.Error())
		}
		out := &struct {
			Body struct {
				Status string               `json:"status"`
				Quotas []*domain.AgentQuota `json:"quotas"`
			}
		}{}
		out.Body.Status = "running"
		out.Body.Quotas = quotas
		return out, nil
	})

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "close-platform-experiment", Method: "POST", Path: "/platform-experiments/{id}/close",
		Summary: "Close a platform experiment", Tags: []string{"platform-experiments"},
		Description: "Terminal: every still-running job is evicted (reason experiment_closed) and " +
			"no further submission is accepted. Optional top_results records the final standings; " +
			"omitted, standings are derived from observed metrics. Safe to call twice -- a second " +
			"call reports invalid_transition rather than changing anything.",
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Body *struct {
			TopResults []AgentResult `json:"top_results,omitempty" doc:"Best-first final standings (up to 3). Omit to derive standings from the metrics store."`
		}
	}) (*struct {
		Body struct {
			Status string `json:"status"`
		}
	}, error) {
		if err := access.PlatformExperiment(ctx, in.ID); err != nil {
			return nil, err
		}
		var topResults []AgentResult
		if in.Body != nil {
			topResults = in.Body.TopResults
		}
		if err := peh.svc.Close(ctx, in.ID, topResults); err != nil {
			return nil, huma.Error400BadRequest(err.Error())
		}
		out := &struct {
			Body struct {
				Status string `json:"status"`
			}
		}{}
		out.Body.Status = "closed"
		return out, nil
	})

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "get-platform-experiment-results", Method: "GET", Path: "/platform-experiments/{id}/results",
		Summary: "Final standings and operator summary", Tags: []string{"platform-experiments"},
		Description: "Per-metric standings, best-first, derived from the metrics store on every read " +
			"(never frozen into PostgreSQL), plus the operator's narrative summary. bests lists, for every " +
			"declared metric whatever its role, each reporting agent's best raw value and the job that set it, unranked.",
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*struct{ Body *PlatformExperimentResults }, error) {
		if err := access.PlatformExperiment(ctx, in.ID); err != nil {
			return nil, err
		}
		results, err := peh.svc.Results(ctx, in.ID)
		if err != nil {
			return nil, huma.Error404NotFound(err.Error())
		}
		return &struct{ Body *PlatformExperimentResults }{Body: results}, nil
	})

	apidocs.Register(doc, apidocs.AudienceCoordinator, apidocs.AfterClose(huma.Operation{
		OperationID: "set-platform-experiment-summary", Method: "POST", Path: "/platform-experiments/{id}/summary",
		Summary: "Record the operator's narrative verdict on a run", Tags: []string{"platform-experiments"},
		Description: "Writable after the experiment closes — this is the one field whose whole point " +
			"is to be written once the run is over and the results are in.",
	}), func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Body struct {
			Summary string `json:"summary"`
		}
	}) (*struct {
		Body struct {
			Status string `json:"status"`
		}
	}, error) {
		if err := access.PlatformExperiment(ctx, in.ID); err != nil {
			return nil, err
		}
		if err := peh.svc.SetSummary(ctx, in.ID, in.Body.Summary); err != nil {
			return nil, huma.Error400BadRequest(err.Error())
		}
		out := &struct {
			Body struct {
				Status string `json:"status"`
			}
		}{}
		out.Body.Status = "summary_recorded"
		return out, nil
	})

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "list-platform-experiment-quotas", Method: "GET", Path: "/platform-experiments/{id}/quotas",
		Summary: "List per-agent quotas for a platform experiment", Tags: []string{"platform-experiments"},
		Description: "Guaranteed/burst accelerator-hours, used vs available, per signed-up agent.",
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*struct{ Body []*domain.AgentQuota }, error) {
		if err := access.PlatformExperiment(ctx, in.ID); err != nil {
			return nil, err
		}
		quotas, err := peh.svc.ListQuotas(ctx, in.ID)
		if errors.Is(err, ErrPlatformExperimentNotFound) {
			return nil, huma.Error404NotFound(err.Error())
		}
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		if quotas == nil {
			quotas = []*domain.AgentQuota{}
		}
		return &struct{ Body []*domain.AgentQuota }{Body: quotas}, nil
	})

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "get-agent-quota", Method: "GET", Path: "/platform-experiments/{id}/quotas/{agentID}",
		Summary: "Get one agent's quota within a platform experiment", Tags: []string{"platform-experiments"},
		Description: "One row of what GET /platform-experiments/{id}/quotas returns for every agent.",
	}, func(ctx context.Context, in *struct {
		ID      string `path:"id"`
		AgentID string `path:"agentID"`
	}) (*struct{ Body *domain.AgentQuota }, error) {
		if err := access.PlatformExperiment(ctx, in.ID); err != nil {
			return nil, err
		}
		aq, err := peh.svc.GetAgentQuota(ctx, in.AgentID, in.ID)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		if aq == nil {
			return nil, huma.Error404NotFound("agent " + in.AgentID + " has no quota in platform experiment " + in.ID + ": quota is granted when the experiment starts, to the agents signed up to it")
		}
		return &struct{ Body *domain.AgentQuota }{Body: aq}, nil
	})

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "get-stages", Method: "GET", Path: "/platform-experiments/{id}/stages",
		Summary: "Get the stage ladder and current standing of a platform experiment", Tags: []string{"platform-experiments"},
		Description: "The ladder, which stage is running, progress through it, and which signed-up agents are still active vs cut. Each stage carries max_job_hours: the longest one job may run while it is current (0/absent = unlimited). Where the next cut falls is not shown; every agent's best value is, at GET /platform-experiments/{id}/results.",
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*struct{ Body StagesStatusResponse }, error) {
		if err := access.PlatformExperiment(ctx, in.ID); err != nil {
			return nil, err
		}
		pe, err := peh.svc.store.GetPlatformExperiment(ctx, in.ID)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		if pe == nil {
			return nil, huma.Error404NotFound("platform experiment not found")
		}
		cuts, err := peh.svc.store.ListCutAgents(ctx, in.ID)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		if cuts == nil {
			cuts = []domain.AgentCut{}
		}
		cutSet := make(map[string]bool, len(cuts))
		for _, c := range cuts {
			cutSet[c.AgentID] = true
		}
		advances, err := peh.svc.store.ListStageAdvances(ctx, in.ID)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		if advances == nil {
			advances = []domain.StageAdvance{}
		}
		allSignups, err := peh.svc.store.ListSignups(ctx, in.ID)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		activeAgents := []string{}
		for _, a := range allSignups {
			if !cutSet[a] {
				activeAgents = append(activeAgents, a)
			}
		}
		progress, err := peh.svc.stageProgress(ctx, pe)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		return &struct{ Body StagesStatusResponse }{Body: StagesStatusResponse{
			Stages:               pe.Stages,
			CurrentStage:         pe.CurrentStage,
			Progress:             progress,
			NextBoundaryProgress: domain.BoundaryProgress(pe.Stages, pe.CurrentStage),
			Advances:             advances,
			ActiveAgents:         activeAgents,
			CutAgents:            cuts,
		}}, nil
	})

	// ---- donations ----

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "list-donations", Method: "GET", Path: "/donations",
		Summary: "List donation requests", Tags: []string{"donations"},
		Description: "Optional ?platform_experiment_id and ?status filters, ?limit (default 20, max 200) and ?offset, most recent " +
			"first. Total match count is returned in the X-Total-Count response header.",
	}, func(ctx context.Context, in *struct {
		PlatformExperimentID string `query:"platform_experiment_id" doc:"Only requests within this platform experiment."`
		Status               string `query:"status" doc:"open, fulfilled or cancelled."`
		ProjectID            string `query:"project_id" doc:"Only requests within this project's platform experiments."`
		Limit                int    `query:"limit" doc:"Page size, default 20, maximum 200. The default is small because the whole response is meant to be read by an agent: ask for more deliberately, using X-Total-Count to decide."`
		Offset               int    `query:"offset" doc:"Rows to skip, for paging through X-Total-Count total matches."`
	}) (*struct {
		Body       []*domain.DonationRequest
		TotalCount int `header:"X-Total-Count"`
	}, error) {
		if in.Limit < 0 || in.Offset < 0 {
			return nil, huma.Error400BadRequest("limit and offset must not be negative")
		}
		scope, err := access.ListScope(ctx, in.ProjectID)
		if err != nil {
			return nil, err
		}
		reqs, err := peh.svc.store.ListDonationRequests(ctx, scope, in.PlatformExperimentID, in.Status, in.Limit, in.Offset)
		if err != nil {
			return nil, huma.Error500InternalServerError("failed to list donations")
		}
		if reqs == nil {
			reqs = []*domain.DonationRequest{}
		}
		total, err := peh.svc.store.CountDonationRequests(ctx, scope, in.PlatformExperimentID, in.Status)
		if err != nil {
			return nil, huma.Error500InternalServerError("failed to count donations")
		}
		return &struct {
			Body       []*domain.DonationRequest
			TotalCount int `header:"X-Total-Count"`
		}{Body: reqs, TotalCount: total}, nil
	})

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "create-donation", Method: "POST", Path: "/donations",
		Summary: "Post a donation request", Tags: []string{"donations"},
		DefaultStatus: 201,
		Description:   "Ask peers within a platform experiment for extra quota (credits transferred between agents).",
	}, func(ctx context.Context, in *struct {
		Body struct {
			AgentID              string  `json:"agent_id"`
			PlatformExperimentID string  `json:"platform_experiment_id"`
			CreditsWant          float64 `json:"credits_want"`
			Reason               string  `json:"reason"`
		}
	}) (*struct{ Body *domain.DonationRequest }, error) {
		b := in.Body
		if b.AgentID == "" || b.PlatformExperimentID == "" || b.CreditsWant <= 0 || b.Reason == "" {
			return nil, huma.Error400BadRequest("agent_id, platform_experiment_id, credits_want (>0), and reason are required")
		}
		if err := access.PlatformExperiment(ctx, b.PlatformExperimentID); err != nil {
			return nil, err
		}
		if err := access.Agent(ctx, b.AgentID); err != nil {
			return nil, err
		}
		if err := peh.svc.RequireActive(ctx, b.PlatformExperimentID, b.AgentID); err != nil {
			return nil, err
		}
		now := time.Now().UTC()
		req := &domain.DonationRequest{
			ID: uuid.New().String(), AgentID: b.AgentID, PlatformExperimentID: b.PlatformExperimentID,
			CreditsWant: b.CreditsWant, Reason: b.Reason, Status: "open", CreatedAt: now, UpdatedAt: now,
		}
		if err := peh.svc.store.CreateDonationRequest(ctx, req); err != nil {
			return nil, huma.Error500InternalServerError("failed to create donation request")
		}
		return &struct{ Body *domain.DonationRequest }{Body: req}, nil
	})

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "cancel-donation", Method: "POST", Path: "/donations/{id}/cancel",
		Summary: "Cancel an open donation request", Tags: []string{"donations"},
		Description: "Withdraws a request that has not been fulfilled. Only an 'open' request can " +
			"be cancelled: a fulfilled one already moved quota and cannot be rewound here.",
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*struct {
		Body struct {
			Status string `json:"status"`
		}
	}, error) {
		if err := access.OwnDonation(ctx, in.ID); err != nil {
			return nil, err
		}
		if err := peh.svc.store.UpdateDonationStatus(ctx, in.ID, "cancelled"); err != nil {
			return nil, huma.Error500InternalServerError("failed to cancel donation request")
		}
		out := &struct {
			Body struct {
				Status string `json:"status"`
			}
		}{}
		out.Body.Status = "cancelled"
		return out, nil
	})

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "fulfill-donation", Method: "POST", Path: "/donations/{id}/fulfill",
		Summary: "Fulfill another agent's donation request", Tags: []string{"donations"},
		Description: "Donor transfers credits_want from their experiment quota to the recipient's.",
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Body struct {
			DonorAgentID string `json:"donor_agent_id"`
		}
	}) (*struct {
		Body struct {
			Status string `json:"status"`
		}
	}, error) {
		if in.Body.DonorAgentID == "" {
			return nil, huma.Error400BadRequest("donor_agent_id is required")
		}
		if err := access.Donation(ctx, in.ID); err != nil {
			return nil, err
		}
		if err := access.Agent(ctx, in.Body.DonorAgentID); err != nil {
			return nil, err
		}
		if err := peh.svc.FulfillDonation(ctx, in.ID, in.Body.DonorAgentID); err != nil {
			var donErr *DonationError
			if errors.As(err, &donErr) {
				return nil, huma.NewError(donationHTTPStatus(donErr.Reason), donErr.Message)
			}
			return nil, huma.Error500InternalServerError(err.Error())
		}
		out := &struct {
			Body struct {
				Status string `json:"status"`
			}
		}{}
		out.Body.Status = "fulfilled"
		return out, nil
	})

	// ---- resource catalog ----

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "get-resource-catalog", Method: "GET", Path: "/resource-catalog",
		Summary: "Get accelerator/CPU/RAM/storage billing rates", Tags: []string{"resource-catalog"},
		Description: "Static reference data: which accelerator types exist and their billing rates. Listed here does NOT mean any cluster has one free — see /resource-catalog/capacity.",
	}, func(ctx context.Context, _ *struct{}) (*struct{ Body ResourceCatalog }, error) {
		return &struct{ Body ResourceCatalog }{Body: peh.catalog}, nil
	})

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "get-resource-capacity", Method: "GET", Path: "/resource-catalog/capacity",
		Summary: "Get live per-cluster accelerator capacity", Tags: []string{"resource-catalog"},
		Description: "Live {accelerator_type, available, total} per cluster the jobs of one org can be placed on: the org's own " +
			"clusters and the ones the platform shares. The org is the one your token is bound to: every API key, agent and " +
			"coordinator token has exactly one, and so does a person in one org. A person in several orgs names it with org_id, " +
			"and is answered 400 without it. An org you do not reach is not found. Staff read one org with org_id and every " +
			"cluster that reports without it. accelerator_type is the exact " +
			"driver-published string a job spec takes, so what you read here is what you submit. Check before " +
			"choosing: a type with no live capacity anywhere QUEUES FOREVER and never errors. Only " +
			"currently-schedulable nodes count — powered-off hardware never appears."}, func(ctx context.Context, in *struct {
		OrgID string `query:"org_id" doc:"The org whose clusters to read. Required only when your token reaches more than one org."`
	}) (*struct {
		Body struct {
			Clusters []ClusterCapacity `json:"clusters"`
		}
	}, error) {
		orgID, every, err := access.PlacementOrg(ctx, in.OrgID)
		if err != nil {
			return nil, err
		}
		clusters, err := peh.liveCapacity(ctx, orgID, every)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		out := &struct {
			Body struct {
				Clusters []ClusterCapacity `json:"clusters"`
			}
		}{}
		out.Body.Clusters = clusters
		return out, nil
	})
}

// StagesStatusResponse is the response body for GET /platform-experiments/{id}/stages.
// Boundaries are published ahead so agents can plan. The cut line is not here; the standings
// are public at GET /platform-experiments/{id}/results.
type StagesStatusResponse struct {
	Stages       []domain.Stage `json:"stages"`
	CurrentStage int            `json:"current_stage"`
	// Progress is max(budget_consumed / total_budget, elapsed / duration) — the single clock
	// driving every boundary.
	Progress             float64 `json:"progress"`
	NextBoundaryProgress float64 `json:"next_boundary_progress"`
	// Advances records each boundary already crossed and when.
	Advances     []domain.StageAdvance `json:"advances"`
	ActiveAgents []string              `json:"active_agents"`
	CutAgents    []domain.AgentCut     `json:"cut_agents"`
}
