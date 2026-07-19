package quota

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
	"go.uber.org/zap"
)

// donationHTTPStatus maps a DonationError reason to the appropriate HTTP status code —
// mirrors scheduler.admissionHTTPStatus.
func donationHTTPStatus(reason string) int {
	switch reason {
	case DonationReasonNotFound:
		return http.StatusNotFound
	case DonationReasonInsufficientQuota:
		return http.StatusPaymentRequired
	case DonationReasonInvalidState, DonationReasonSelfDonation:
		return http.StatusConflict
	default:
		return http.StatusUnprocessableEntity
	}
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
		var donErr *DonationError
		if errors.As(err, &donErr) {
			// Expected, client-triggerable outcome (already fulfilled/cancelled, self-donation,
			// insufficient quota) — not a server fault, so don't log at Error level or return 500.
			respondError(w, donationHTTPStatus(donErr.Reason), donErr.Message)
			return
		}
		h.logger.Error("fulfillDonation failed", zap.String("id", id), zap.Error(err))
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "fulfilled"})
}
