package quota

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
)

// Handler wires the quota Service to HTTP endpoints via a chi router.
type Handler struct {
	svc    *Service
	logger *zap.Logger
}

// NewHandler constructs a Handler.
func NewHandler(svc *Service, logger *zap.Logger) *Handler {
	return &Handler{svc: svc, logger: logger}
}

// RegisterRoutes mounts the quota endpoints on r.
func (h *Handler) RegisterRoutes(r chi.Router) {
	r.Post("/agents", h.registerAgent)
	r.Get("/agents", h.listAgents)
	r.Get("/balances", h.listBalances)
	r.Get("/ledger/{agentID}", h.getAgentLedger)
}

// respondJSON encodes v as JSON and writes it to w with the given status code.
func respondJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// respondError writes a JSON error response.
func respondError(w http.ResponseWriter, status int, msg string) {
	respondJSON(w, status, map[string]string{"error": msg})
}

// POST /agents
func (h *Handler) registerAgent(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.ID == "" || req.Name == "" {
		respondError(w, http.StatusBadRequest, "id and name are required")
		return
	}
	agent, err := h.svc.RegisterAgent(r.Context(), req.ID, req.Name)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") || strings.Contains(err.Error(), "unique constraint") {
			respondError(w, http.StatusConflict, "agent already exists: "+req.ID)
			return
		}
		h.logger.Error("registerAgent failed", zap.String("id", req.ID), zap.Error(err))
		respondError(w, http.StatusInternalServerError, "failed to register agent: "+err.Error())
		return
	}
	respondJSON(w, http.StatusCreated, agent)
}

// GET /agents
func (h *Handler) listAgents(w http.ResponseWriter, r *http.Request) {
	agents, err := h.svc.store.ListAgents(r.Context())
	if err != nil {
		h.logger.Error("listAgents failed", zap.Error(err))
		respondError(w, http.StatusInternalServerError, "failed to list agents")
		return
	}
	respondJSON(w, http.StatusOK, agents)
}

// GET /balances
func (h *Handler) listBalances(w http.ResponseWriter, r *http.Request) {
	balances, err := h.svc.ListBalances(r.Context())
	if err != nil {
		h.logger.Error("listBalances failed", zap.Error(err))
		respondError(w, http.StatusInternalServerError, "failed to list balances")
		return
	}
	respondJSON(w, http.StatusOK, balances)
}

// GET /ledger/{agentID}
func (h *Handler) getAgentLedger(w http.ResponseWriter, r *http.Request) {
	agentID := chi.URLParam(r, "agentID")
	entries, err := h.svc.GetAgentLedger(r.Context(), agentID)
	if err != nil {
		h.logger.Error("getAgentLedger failed", zap.String("agent_id", agentID), zap.Error(err))
		respondError(w, http.StatusInternalServerError, "failed to get ledger")
		return
	}
	respondJSON(w, http.StatusOK, entries)
}
