package quota

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// PlatformExperimentsHandler exposes the Platform Experiment lifecycle over HTTP.
type PlatformExperimentsHandler struct {
	svc     *PlatformExperimentsService
	logger  *zap.Logger
	catalog ResourceCatalog
}

// NewPlatformExperimentsHandler constructs the handler.
func NewPlatformExperimentsHandler(svc *PlatformExperimentsService, logger *zap.Logger) *PlatformExperimentsHandler {
	return &PlatformExperimentsHandler{svc: svc, logger: logger}
}

// WithCatalog attaches the resource catalog served by GET /resource-catalog.
func (h *PlatformExperimentsHandler) WithCatalog(catalog ResourceCatalog) *PlatformExperimentsHandler {
	h.catalog = catalog
	return h
}

// RegisterRoutes mounts platform experiment endpoints on r.
func (h *PlatformExperimentsHandler) RegisterRoutes(r chi.Router) {
	r.Post("/platform-experiments", h.create)
	r.Get("/platform-experiments", h.list)
	r.Get("/platform-experiments/{id}", h.get)
	r.Put("/platform-experiments/{id}", h.update)
	r.Post("/platform-experiments/{id}/signup", h.signup)
	r.Post("/platform-experiments/{id}/start", h.start)
	r.Post("/platform-experiments/{id}/close", h.close)
	r.Get("/platform-experiments/{id}/quotas", h.listQuotas)
	r.Get("/quota/{agentID}/experiment/{experimentID}", h.getAgentQuota)
	r.Get("/platform-experiments/{id}/phase2-status", h.getPhase2Status)
	// Donation board (experiment-scoped — transfers quota between agents).
	r.Get("/donations", h.listDonations)
	r.Post("/donations", h.createDonation)
	r.Post("/donations/{id}/cancel", h.cancelDonation)
	r.Post("/donations/{id}/fulfill", h.fulfillDonation)
	// Resource catalog — read-only reference data (GPU type rates, CPU/RAM/storage rates)
	// so UIs/agents never need to hardcode a copy of the operator's config.
	r.Get("/resource-catalog", h.getResourceCatalog)
}

// GPUTypeInfo is the agent/UI-facing view of one catalog GPU type — pricing only, not the
// execution-engine internals (resource name, taint key, node label key/value) which stay
// entirely within the backend/workload layer.
type GPUTypeInfo struct {
	Name    string  `json:"name"`
	T4HRate float64 `json:"t4h_rate"`
}

// ResourceCatalog is the full set of resource-pricing reference data served by
// GET /resource-catalog.
type ResourceCatalog struct {
	GPUTypes          []GPUTypeInfo `json:"gpu_types"`
	CPUCoreHourRate   float64       `json:"cpu_core_hour_rate"`
	RAMGBHourRate     float64       `json:"ram_gb_hour_rate"`
	StorageGBHourRate float64       `json:"storage_gb_hour_rate"`
}

func (h *PlatformExperimentsHandler) getResourceCatalog(w http.ResponseWriter, r *http.Request) {
	respondJSON(w, http.StatusOK, h.catalog)
}

func (h *PlatformExperimentsHandler) create(w http.ResponseWriter, r *http.Request) {
	var req CreatePlatformExperimentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Name == "" || req.BudgetT4Hours <= 0 {
		respondError(w, http.StatusBadRequest, "name and budget_t4_hours are required")
		return
	}

	pe, err := h.svc.Create(r.Context(), req)
	if err != nil {
		h.logger.Error("create platform experiment", zap.Error(err))
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	respondJSON(w, http.StatusCreated, pe)
}

func (h *PlatformExperimentsHandler) list(w http.ResponseWriter, r *http.Request) {
	statusFilter := r.URL.Query().Get("status")
	pes, err := h.svc.store.ListPlatformExperiments(r.Context(), statusFilter)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if pes == nil {
		pes = []*domain.PlatformExperiment{}
	}
	respondJSON(w, http.StatusOK, pes)
}

func (h *PlatformExperimentsHandler) get(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	pe, err := h.svc.store.GetPlatformExperiment(r.Context(), id)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if pe == nil {
		respondError(w, http.StatusNotFound, "platform experiment not found")
		return
	}

	signups, _ := h.svc.store.ListSignups(r.Context(), id)
	pe.SignedUpAgents = signups
	pe.SignupCount = len(signups)
	respondJSON(w, http.StatusOK, pe)
}

func (h *PlatformExperimentsHandler) update(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req CreatePlatformExperimentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	pe, err := h.svc.Update(r.Context(), id, req)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	respondJSON(w, http.StatusOK, pe)
}

func (h *PlatformExperimentsHandler) signup(w http.ResponseWriter, r *http.Request) {
	platformExpID := chi.URLParam(r, "id")
	var body struct {
		AgentID string `json:"agent_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if body.AgentID == "" {
		respondError(w, http.StatusBadRequest, "agent_id is required")
		return
	}

	if err := h.svc.Signup(r.Context(), platformExpID, body.AgentID); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{
			"reason":  err.Error(),
			"message": err.Error(),
		})
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "signed_up"})
}

func (h *PlatformExperimentsHandler) start(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	quotas, err := h.svc.Start(r.Context(), id)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"status": "running",
		"quotas": quotas,
	})
}

func (h *PlatformExperimentsHandler) close(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var body struct {
		TopResults []AgentResult `json:"top_results"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	if err := h.svc.Close(r.Context(), id, body.TopResults); err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "closed"})
}

func (h *PlatformExperimentsHandler) listQuotas(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	quotas, err := h.svc.ListQuotas(r.Context(), id)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if quotas == nil {
		quotas = []*domain.AgentQuota{}
	}
	respondJSON(w, http.StatusOK, quotas)
}

// GET /platform-experiments/{id}/phase2-status
// Returns the current phase and, in phase 2, which agents are active vs. held.
func (h *PlatformExperimentsHandler) getPhase2Status(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	pe, err := h.svc.store.GetPlatformExperiment(r.Context(), id)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if pe == nil {
		respondError(w, http.StatusNotFound, "platform experiment not found")
		return
	}

	heldAgents, err := h.svc.store.ListPhase2HeldAgents(r.Context(), id)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if heldAgents == nil {
		heldAgents = []string{}
	}

	// Active = signed-up agents minus held agents.
	allSignups, _ := h.svc.store.ListSignups(r.Context(), id)
	heldSet := make(map[string]bool, len(heldAgents))
	for _, a := range heldAgents {
		heldSet[a] = true
	}
	var activeAgents []string
	for _, a := range allSignups {
		if !heldSet[a] {
			activeAgents = append(activeAgents, a)
		}
	}
	if activeAgents == nil {
		activeAgents = []string{}
	}

	type phase2StatusResponse struct {
		Phase             int      `json:"phase"`
		Phase2TriggeredAt *string  `json:"phase2_triggered_at,omitempty"`
		ActiveAgents      []string `json:"active_agents"`
		HeldAgents        []string `json:"held_agents"`
		// BoundaryFraction is the fraction of pe.budget_t4_hours at which phase 2 triggers,
		// as actually configured on this deployment — the UI must not hardcode this.
		BoundaryFraction float64 `json:"boundary_fraction"`
	}

	var triggeredAt *string
	if pe.Phase2TriggeredAt != nil {
		s := pe.Phase2TriggeredAt.Format("2006-01-02T15:04:05Z")
		triggeredAt = &s
	}

	boundaryFraction := h.svc.cfg.Phase1ExploreFraction
	if boundaryFraction == 0 {
		boundaryFraction = domain.Phase1ExploreFraction
	}

	respondJSON(w, http.StatusOK, phase2StatusResponse{
		Phase:             pe.Phase,
		Phase2TriggeredAt: triggeredAt,
		ActiveAgents:      activeAgents,
		HeldAgents:        heldAgents,
		BoundaryFraction:  boundaryFraction,
	})
}

func (h *PlatformExperimentsHandler) getAgentQuota(w http.ResponseWriter, r *http.Request) {
	agentID := chi.URLParam(r, "agentID")
	experimentID := chi.URLParam(r, "experimentID")

	aq, err := h.svc.GetQuota(r.Context(), agentID, experimentID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if aq == nil {
		respondError(w, http.StatusNotFound, "quota not found")
		return
	}
	respondJSON(w, http.StatusOK, aq)
}

// GET /donations — list donation requests. Optional ?status= filter (open|fulfilled|cancelled).
func (h *PlatformExperimentsHandler) listDonations(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	reqs, err := h.svc.store.ListDonationRequests(r.Context(), status)
	if err != nil {
		h.logger.Error("listDonations failed", zap.Error(err))
		respondError(w, http.StatusInternalServerError, "failed to list donations")
		return
	}
	if reqs == nil {
		reqs = []*domain.DonationRequest{}
	}
	respondJSON(w, http.StatusOK, reqs)
}

// POST /donations — agent posts a request for extra compute from peers within an experiment.
// Body: {"agent_id": "...", "platform_experiment_id": "...", "credits_want": 50, "reason": "..."}
func (h *PlatformExperimentsHandler) createDonation(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AgentID              string  `json:"agent_id"`
		PlatformExperimentID string  `json:"platform_experiment_id"`
		CreditsWant          float64 `json:"credits_want"`
		Reason               string  `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if body.AgentID == "" || body.PlatformExperimentID == "" || body.CreditsWant <= 0 || body.Reason == "" {
		respondError(w, http.StatusBadRequest, "agent_id, platform_experiment_id, credits_want (>0), and reason are required")
		return
	}
	now := time.Now().UTC()
	req := &domain.DonationRequest{
		ID:                   uuid.New().String(),
		AgentID:              body.AgentID,
		PlatformExperimentID: body.PlatformExperimentID,
		CreditsWant:          body.CreditsWant,
		Reason:               body.Reason,
		Status:               "open",
		CreatedAt:            now,
		UpdatedAt:            now,
	}
	if err := h.svc.store.CreateDonationRequest(r.Context(), req); err != nil {
		h.logger.Error("createDonation failed", zap.Error(err))
		respondError(w, http.StatusInternalServerError, "failed to create donation request")
		return
	}
	respondJSON(w, http.StatusCreated, req)
}

// POST /donations/{id}/cancel — agent cancels their own open donation request.
func (h *PlatformExperimentsHandler) cancelDonation(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := h.svc.store.UpdateDonationStatus(r.Context(), id, "cancelled"); err != nil {
		h.logger.Error("cancelDonation failed", zap.String("id", id), zap.Error(err))
		respondError(w, http.StatusInternalServerError, "failed to cancel donation request")
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
}

// POST /donations/{id}/fulfill — donor fulfills another agent's donation request.
// Body: {"donor_agent_id": "..."}
// Transfers credits_want from donor's experiment quota to recipient's experiment quota.
func (h *PlatformExperimentsHandler) fulfillDonation(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var body struct {
		DonorAgentID string `json:"donor_agent_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if body.DonorAgentID == "" {
		respondError(w, http.StatusBadRequest, "donor_agent_id is required")
		return
	}
	if err := h.svc.FulfillDonation(r.Context(), id, body.DonorAgentID); err != nil {
		h.logger.Error("fulfillDonation failed", zap.String("id", id), zap.Error(err))
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "fulfilled"})
}
