package quota

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
)

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
		// BoundaryFraction is the fraction of pe.budget_accelerator_hours at which phase 2 triggers,
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
