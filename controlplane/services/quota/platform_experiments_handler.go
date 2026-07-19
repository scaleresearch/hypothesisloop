package quota

import (
	"net/http"

	"github.com/go-chi/chi/v5"
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
	// Resource catalog — read-only reference data (Accelerator type rates, CPU/RAM/storage rates)
	// so UIs/agents never need to hardcode a copy of the operator's config.
	r.Get("/resource-catalog", h.getResourceCatalog)
}

// AcceleratorTypeInfo is the agent/UI-facing view of one catalog accelerator type — pricing only, not the
// execution-engine internals (resource name, taint key, node label key/value) which stay
// entirely within the backend/workload layer.
type AcceleratorTypeInfo struct {
	Name    string  `json:"name"`
	AccHRate float64 `json:"acch_rate"`
}

// ResourceCatalog is the full set of resource-pricing reference data served by
// GET /resource-catalog.
type ResourceCatalog struct {
	AcceleratorTypes          []AcceleratorTypeInfo `json:"accelerator_types"`
	CPUCoreHourRate   float64       `json:"cpu_core_hour_rate"`
	RAMGBHourRate     float64       `json:"ram_gb_hour_rate"`
	StorageGBHourRate float64       `json:"storage_gb_hour_rate"`
}

func (h *PlatformExperimentsHandler) getResourceCatalog(w http.ResponseWriter, r *http.Request) {
	respondJSON(w, http.StatusOK, h.catalog)
}
