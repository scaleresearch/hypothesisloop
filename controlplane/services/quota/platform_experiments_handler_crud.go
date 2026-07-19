package quota

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
	"go.uber.org/zap"
)

func (h *PlatformExperimentsHandler) create(w http.ResponseWriter, r *http.Request) {
	var req CreatePlatformExperimentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Name == "" || req.BudgetAcceleratorHours <= 0 {
		respondError(w, http.StatusBadRequest, "name and budget_accelerator_hours are required")
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
