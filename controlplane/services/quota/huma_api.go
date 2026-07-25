package quota

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
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

// RegisterHuma registers every quota-service operation (agents/balances/ledger,
// platform-experiments, donations, resource catalog) on the shared Huma doc.
func RegisterHuma(doc *apidocs.Doc, h *Handler, peh *PlatformExperimentsHandler) {
	// ---- agents / balances / ledger ----

	apidocs.Register(doc, huma.Operation{
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

	apidocs.Register(doc, huma.Operation{
		OperationID: "list-agents", Method: "GET", Path: "/agents",
		Summary: "List registered agents", Tags: []string{"agents"},
	}, func(ctx context.Context, _ *struct{}) (*struct{ Body []*domain.Agent }, error) {
		agents, err := h.svc.store.ListAgents(ctx)
		if err != nil {
			return nil, huma.Error500InternalServerError("failed to list agents")
		}
		return &struct{ Body []*domain.Agent }{Body: agents}, nil
	})

	apidocs.Register(doc, huma.Operation{
		OperationID: "list-balances", Method: "GET", Path: "/balances",
		Summary: "List agent credit balances", Tags: []string{"agents"},
	}, func(ctx context.Context, _ *struct{}) (*struct{ Body []*domain.AgentBalance }, error) {
		balances, err := h.svc.ListBalances(ctx)
		if err != nil {
			return nil, huma.Error500InternalServerError("failed to list balances")
		}
		return &struct{ Body []*domain.AgentBalance }{Body: balances}, nil
	})

	apidocs.Register(doc, huma.Operation{
		OperationID: "get-agent-ledger", Method: "GET", Path: "/ledger/{agentID}",
		Summary: "Get an agent's credit ledger", Tags: []string{"agents"},
	}, func(ctx context.Context, in *struct {
		AgentID string `path:"agentID"`
	}) (*struct{ Body []*domain.CreditLedgerEntry }, error) {
		entries, err := h.svc.GetAgentLedger(ctx, in.AgentID)
		if err != nil {
			return nil, huma.Error500InternalServerError("failed to get ledger")
		}
		return &struct{ Body []*domain.CreditLedgerEntry }{Body: entries}, nil
	})

	// ---- platform experiments ----

	apidocs.Register(doc, huma.Operation{
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
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		return &struct{ Body *domain.PlatformExperiment }{Body: pe}, nil
	})

	apidocs.Register(doc, huma.Operation{
		OperationID: "list-platform-experiments", Method: "GET", Path: "/platform-experiments",
		Summary: "List platform experiments", Tags: []string{"platform-experiments"},
	}, func(ctx context.Context, in *struct {
		Status string `query:"status" doc:"Optional status filter"`
	}) (*struct{ Body []*domain.PlatformExperiment }, error) {
		pes, err := peh.svc.store.ListPlatformExperiments(ctx, in.Status)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		if pes == nil {
			pes = []*domain.PlatformExperiment{}
		}
		return &struct{ Body []*domain.PlatformExperiment }{Body: pes}, nil
	})

	apidocs.Register(doc, huma.Operation{
		OperationID: "get-platform-experiment", Method: "GET", Path: "/platform-experiments/{id}",
		Summary: "Get a platform experiment", Tags: []string{"platform-experiments"},
		Description: "Returns status, budget, phase and the list of signed-up agents.",
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

	apidocs.Register(doc, huma.Operation{
		OperationID: "update-platform-experiment", Method: "PUT", Path: "/platform-experiments/{id}",
		Summary: "Update a platform experiment", Tags: []string{"platform-experiments"},
		Description: "Amends an experiment while its status is open — including `metrics`, so a ranking " +
			"metric found to be mis-specified can be corrected in place instead of closing the " +
			"experiment and discarding every agent's registered hypotheses and baseline work. " +
			"Rejected once the status is no longer open.",
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

	apidocs.Register(doc, huma.Operation{
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

	apidocs.Register(doc, huma.Operation{
		OperationID: "start-platform-experiment", Method: "POST", Path: "/platform-experiments/{id}/start",
		Summary: "Start a platform experiment", Tags: []string{"platform-experiments"},
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

	apidocs.Register(doc, huma.Operation{
		OperationID: "close-platform-experiment", Method: "POST", Path: "/platform-experiments/{id}/close",
		Summary: "Close a platform experiment", Tags: []string{"platform-experiments"},
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Body struct {
			TopResults []AgentResult `json:"top_results"`
		}
	}) (*struct {
		Body struct {
			Status string `json:"status"`
		}
	}, error) {
		if err := peh.svc.Close(ctx, in.ID, in.Body.TopResults); err != nil {
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

	apidocs.Register(doc, huma.Operation{
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

	apidocs.Register(doc, huma.Operation{
		OperationID: "get-agent-quota", Method: "GET", Path: "/quota/{agentID}/experiment/{experimentID}",
		Summary: "Get one agent's quota within a platform experiment", Tags: []string{"platform-experiments"},
	}, func(ctx context.Context, in *struct {
		AgentID      string `path:"agentID"`
		ExperimentID string `path:"experimentID"`
	}) (*struct{ Body *domain.AgentQuota }, error) {
		aq, err := peh.svc.GetQuota(ctx, in.AgentID, in.ExperimentID)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		if aq == nil {
			return nil, huma.Error404NotFound("quota not found")
		}
		return &struct{ Body *domain.AgentQuota }{Body: aq}, nil
	})

	apidocs.Register(doc, huma.Operation{
		OperationID: "get-phase2-status", Method: "GET", Path: "/platform-experiments/{id}/phase2-status",
		Summary: "Get phase-2 status for a platform experiment", Tags: []string{"platform-experiments"},
		Description: "Reports current phase and, in phase 2, which signed-up agents are active vs held (evicted, blocked from resubmitting). boundary_fraction is the configured budget fraction at which phase 2 triggers — never hardcode it.",
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*struct{ Body Phase2StatusResponse }, error) {
		pe, err := peh.svc.store.GetPlatformExperiment(ctx, in.ID)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		if pe == nil {
			return nil, huma.Error404NotFound("platform experiment not found")
		}
		heldAgents, err := peh.svc.store.ListPhase2HeldAgents(ctx, in.ID)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		if heldAgents == nil {
			heldAgents = []string{}
		}
		allSignups, _ := peh.svc.store.ListSignups(ctx, in.ID)
		heldSet := make(map[string]bool, len(heldAgents))
		for _, a := range heldAgents {
			heldSet[a] = true
		}
		activeAgents := []string{}
		for _, a := range allSignups {
			if !heldSet[a] {
				activeAgents = append(activeAgents, a)
			}
		}
		var triggeredAt *string
		if pe.Phase2TriggeredAt != nil {
			s := pe.Phase2TriggeredAt.Format("2006-01-02T15:04:05Z")
			triggeredAt = &s
		}
		boundaryFraction := peh.svc.cfg.Phase1ExploreFraction
		if boundaryFraction == 0 {
			boundaryFraction = domain.Phase1ExploreFraction
		}
		return &struct{ Body Phase2StatusResponse }{Body: Phase2StatusResponse{
			Phase:             pe.Phase,
			Phase2TriggeredAt: triggeredAt,
			ActiveAgents:      activeAgents,
			HeldAgents:        heldAgents,
			BoundaryFraction:  boundaryFraction,
		}}, nil
	})

	// ---- donations ----

	apidocs.Register(doc, huma.Operation{
		OperationID: "list-donations", Method: "GET", Path: "/donations",
		Summary: "List donation requests", Tags: []string{"donations"},
	}, func(ctx context.Context, in *struct {
		Status string `query:"status" doc:"Optional filter: open|fulfilled|cancelled"`
	}) (*struct{ Body []*domain.DonationRequest }, error) {
		reqs, err := peh.svc.store.ListDonationRequests(ctx, in.Status)
		if err != nil {
			return nil, huma.Error500InternalServerError("failed to list donations")
		}
		if reqs == nil {
			reqs = []*domain.DonationRequest{}
		}
		return &struct{ Body []*domain.DonationRequest }{Body: reqs}, nil
	})

	apidocs.Register(doc, huma.Operation{
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

	apidocs.Register(doc, huma.Operation{
		OperationID: "cancel-donation", Method: "POST", Path: "/donations/{id}/cancel",
		Summary: "Cancel an open donation request", Tags: []string{"donations"},
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

	apidocs.Register(doc, huma.Operation{
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

	apidocs.Register(doc, huma.Operation{
		OperationID: "get-resource-catalog", Method: "GET", Path: "/resource-catalog",
		Summary: "Get accelerator/CPU/RAM/storage billing rates", Tags: []string{"resource-catalog"},
		Description: "Static reference data: which accelerator types exist and their billing rates. Existence here does NOT mean any cluster currently has one free — check /resource-catalog/capacity for that.",
	}, func(ctx context.Context, _ *struct{}) (*struct{ Body ResourceCatalog }, error) {
		return &struct{ Body ResourceCatalog }{Body: peh.catalog}, nil
	})

	apidocs.Register(doc, huma.Operation{
		OperationID: "get-resource-capacity", Method: "GET", Path: "/resource-catalog/capacity",
		Summary: "Get live per-cluster accelerator capacity", Tags: []string{"resource-catalog"},
		Description: "Live {accelerator_type, available, total} per cluster. accelerator_type is the exact string " +
			"a job spec's accelerator_type takes — the value its driver publishes — so what you read here is what " +
			"you submit. Check before choosing: a type with no live capacity anywhere QUEUES FOREVER (never errors). " +
			"Only currently-schedulable nodes count, so powered-off hardware never appears here.",}, func(ctx context.Context, _ *struct{}) (*struct {
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
		available, err := metricsdb.LiveClusterAcceleratorCapacity(ctx, peh.metricsDBURL, peh.capacityFreshness)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		total, err := metricsdb.LiveClusterAcceleratorTotalCapacity(ctx, peh.metricsDBURL, peh.capacityFreshness)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		out.Body.Clusters = buildClusterCapacity(available, total)
		return out, nil
	})
}

// Phase2StatusResponse is the response body for GET /platform-experiments/{id}/phase2-status.
type Phase2StatusResponse struct {
	Phase             int      `json:"phase"`
	Phase2TriggeredAt *string  `json:"phase2_triggered_at,omitempty"`
	ActiveAgents      []string `json:"active_agents"`
	HeldAgents        []string `json:"held_agents"`
	BoundaryFraction  float64  `json:"boundary_fraction"`
}
