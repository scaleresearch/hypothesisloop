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
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
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
	// ---- agents ----

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "register-agent", Method: "POST", Path: "/agents",
		Summary: "Register an agent", Tags: []string{"agents"},
		DefaultStatus: 201,
		Description:   "Idempotent identity registration. Returns 409 if the id already exists. Do this once before signing up to a platform experiment.",
	}, func(ctx context.Context, in *struct {
		Body struct {
			ID   string `json:"id" doc:"Stable unique agent id"`
			Name string `json:"name,omitempty" required:"false" doc:"Human-readable display name; defaults to id"`
		}
	}) (*struct{ Body *domain.Agent }, error) {
		if in.Body.ID == "" {
			return nil, huma.Error400BadRequest("id is required")
		}
		// id is the only identity that matters; name is presentation-only (UI/leaderboard), so
		// requiring it would reject an otherwise complete registration over a cosmetic field.
		name := in.Body.Name
		if name == "" {
			name = in.Body.ID
		}
		agent, err := h.svc.RegisterAgent(ctx, in.Body.ID, name)
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
		Description: "Optional ?limit (default 20, max 200) and ?offset, oldest first. Total " +
			"registered count is returned in the X-Total-Count response header.",
	}, func(ctx context.Context, in *struct {
		Limit  int `query:"limit" doc:"Page size, default 20, maximum 200. The default is small because the whole response is meant to be read by an agent: ask for more deliberately, using X-Total-Count to decide."`
		Offset int `query:"offset" doc:"Rows to skip, for paging through X-Total-Count total matches."`
	}) (*struct {
		Body       []*domain.Agent
		TotalCount int `header:"X-Total-Count"`
	}, error) {
		if in.Limit < 0 || in.Offset < 0 {
			return nil, huma.Error400BadRequest("limit and offset must not be negative")
		}
		agents, err := h.svc.store.ListAgents(ctx, in.Limit, in.Offset)
		if err != nil {
			return nil, huma.Error500InternalServerError("failed to list agents")
		}
		total, err := h.svc.store.CountAgents(ctx)
		if err != nil {
			return nil, huma.Error500InternalServerError("failed to count agents")
		}
		return &struct {
			Body       []*domain.Agent
			TotalCount int `header:"X-Total-Count"`
		}{Body: agents, TotalCount: total}, nil
	})

	// ---- platform experiments ----

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "create-platform-experiment", Method: "POST", Path: "/platform-experiments",
		Summary: "Create a platform experiment", Tags: []string{"platform-experiments"},
		DefaultStatus: 201,
		Description:   "Operator/dashboard endpoint. name and budget_accelerator_hours are required.",
	}, func(ctx context.Context, in *struct {
		Body CreatePlatformExperimentRequest
	}) (*struct{ Body *domain.PlatformExperiment }, error) {
		if in.Body.Name == "" || in.Body.BudgetAcceleratorHours <= 0 {
			return nil, huma.Error400BadRequest("name and budget_accelerator_hours are required")
		}
		pe, err := peh.svc.Create(ctx, in.Body)
		if errors.Is(err, domain.ErrInvalidStages) {
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
		Status string `query:"status" doc:"draft, open, running or closed."`
		Search string `query:"q" doc:"Case-insensitive substring match against name and description."`
		Limit  int    `query:"limit" doc:"Page size, default 20, maximum 200. The default is small because the whole response is meant to be read by an agent: ask for more deliberately, using X-Total-Count to decide."`
		Offset int    `query:"offset" doc:"Rows to skip, for paging through X-Total-Count total matches."`
		Sort   string `query:"sort" doc:"created_at, name or status, optionally prefixed with a minus for descending."`
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
		filter := db.PlatformExperimentsFilter{
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
		pe, err := peh.svc.store.GetPlatformExperiment(ctx, in.ID)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		if pe == nil {
			return nil, huma.Error404NotFound("platform experiment not found")
		}
		signups, _ := peh.svc.store.ListSignups(ctx, in.ID)
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
		pe, err := peh.svc.Update(ctx, in.ID, in.Body)
		if err != nil {
			return nil, huma.Error400BadRequest(err.Error())
		}
		return &struct{ Body *domain.PlatformExperiment }{Body: pe}, nil
	})

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "signup-platform-experiment", Method: "POST", Path: "/platform-experiments/{id}/signup",
		Summary: "Sign up an agent to a platform experiment", Tags: []string{"platform-experiments"},
		Description: "Join before submitting jobs. Every job's metadata.platform_experiment_id must be one you are signed up to.",
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Body struct {
			AgentID string `json:"agent_id"`
		}
	}) (*struct {
		Body struct {
			Status string `json:"status"`
		}
	}, error) {
		if in.Body.AgentID == "" {
			return nil, huma.Error400BadRequest("agent_id is required")
		}
		if err := peh.svc.Signup(ctx, in.ID, in.Body.AgentID); err != nil {
			return nil, &reasonError{status: 400, Reason: err.Error(), Message: err.Error()}
		}
		out := &struct {
			Body struct {
				Status string `json:"status"`
			}
		}{}
		out.Body.Status = "signed_up"
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
			"(never frozen into PostgreSQL), plus the operator's narrative summary.",
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*struct{ Body *PlatformExperimentResults }, error) {
		results, err := peh.svc.Results(ctx, in.ID)
		if err != nil {
			return nil, huma.Error404NotFound(err.Error())
		}
		return &struct{ Body *PlatformExperimentResults }{Body: results}, nil
	})

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "set-platform-experiment-summary", Method: "POST", Path: "/platform-experiments/{id}/summary",
		Summary: "Record the operator's narrative verdict on a run", Tags: []string{"platform-experiments"},
		Description: "Writable after the experiment closes — this is the one field whose whole point " +
			"is to be written once the run is over and the results are in.",
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Body struct {
			Summary string `json:"summary"`
		}
	}) (*struct {
		Body struct {
			Status string `json:"status"`
		}
	}, error) {
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
		quotas, err := peh.svc.ListQuotas(ctx, in.ID)
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
		aq, err := peh.svc.GetQuota(ctx, in.AgentID, in.ID)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		if aq == nil {
			return nil, huma.Error404NotFound("quota not found")
		}
		return &struct{ Body *domain.AgentQuota }{Body: aq}, nil
	})

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "get-stages", Method: "GET", Path: "/platform-experiments/{id}/stages",
		Summary: "Get the stage ladder and current standing of a platform experiment", Tags: []string{"platform-experiments"},
		Description: "The ladder, which stage is running, progress through it, and which signed-up agents are still active vs cut. Each stage carries max_job_hours: the longest one job may run while it is current (0/absent = unlimited). Per-agent rank is deliberately not exposed — you can see whether you are cut, not how close you are.",
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*struct{ Body StagesStatusResponse }, error) {
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
		Description: "Optional ?status filter, ?limit (default 20, max 200) and ?offset, most recent " +
			"first. Total match count is returned in the X-Total-Count response header.",
	}, func(ctx context.Context, in *struct {
		Status string `query:"status" doc:"open, fulfilled or cancelled."`
		Limit  int    `query:"limit" doc:"Page size, default 20, maximum 200. The default is small because the whole response is meant to be read by an agent: ask for more deliberately, using X-Total-Count to decide."`
		Offset int    `query:"offset" doc:"Rows to skip, for paging through X-Total-Count total matches."`
	}) (*struct {
		Body       []*domain.DonationRequest
		TotalCount int `header:"X-Total-Count"`
	}, error) {
		if in.Limit < 0 || in.Offset < 0 {
			return nil, huma.Error400BadRequest("limit and offset must not be negative")
		}
		reqs, err := peh.svc.store.ListDonationRequests(ctx, in.Status, in.Limit, in.Offset)
		if err != nil {
			return nil, huma.Error500InternalServerError("failed to list donations")
		}
		if reqs == nil {
			reqs = []*domain.DonationRequest{}
		}
		total, err := peh.svc.store.CountDonationRequests(ctx, in.Status)
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
		Description: "Live {accelerator_type, available, total} per cluster. accelerator_type is the exact " +
			"driver-published string a job spec takes, so what you read here is what you submit. Check before " +
			"choosing: a type with no live capacity anywhere QUEUES FOREVER and never errors. Only " +
			"currently-schedulable nodes count — powered-off hardware never appears."}, func(ctx context.Context, _ *struct{}) (*struct {
		Body struct {
			Clusters []ClusterCapacity `json:"clusters"`
		}
	}, error) {
		out := &struct {
			Body struct {
				Clusters []ClusterCapacity `json:"clusters"`
			}
		}{}
		out.Body.Clusters = []ClusterCapacity{}
		if peh.metricsDBURL == "" {
			return out, nil
		}
		available, total, err := metricsdb.LiveClusterAcceleratorAvailableAndTotal(ctx, peh.metricsDBURL, peh.capacityFreshness)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		out.Body.Clusters = buildClusterCapacity(available, total)
		return out, nil
	})
}

// StagesStatusResponse is the response body for GET /platform-experiments/{id}/stages.
// Boundaries are published ahead so agents can plan; live rank is not, so agents cannot time
// submissions around the cut line instead of improving their metric.
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
