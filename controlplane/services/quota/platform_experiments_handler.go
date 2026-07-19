package quota

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/scaleresearch/openresearch/controlplane/shared/metricsdb"
)

// PlatformExperimentsHandler exposes the Platform Experiment lifecycle over HTTP.
type PlatformExperimentsHandler struct {
	svc     *PlatformExperimentsService
	logger  *zap.Logger
	catalog ResourceCatalog

	// Live-capacity lookup (GET /resource-catalog/capacity) — nil metricsDBURL means the
	// endpoint responds with an empty capacity list rather than panicking, so tests/callers
	// that never wire WithLiveCapacity still get a well-formed (if uninformative) response.
	metricsDBURL      string
	flavorNameFn      func(flavor string) string
	capacityFreshness time.Duration
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

// WithLiveCapacity attaches what GET /resource-catalog/capacity needs: the metrics DB cluster-agents'
// live per-flavor accelerator capacity reports land in (see metricsdb.LiveClusterAcceleratorCapacity),
// a flavor->accelerator-type-name translator (config.Config.AcceleratorNameForFlavor — capacity is
// recorded under the internal "flavor-*" name, never the agent-facing AcceleratorType name), and the
// freshness window past which a cluster's last report is treated as stale/disconnected (same window
// used elsewhere for cluster-alive gating, e.g. Scheduler.ClusterUnreachableAfterSeconds).
func (h *PlatformExperimentsHandler) WithLiveCapacity(metricsDBURL string, flavorNameFn func(flavor string) string, freshness time.Duration) *PlatformExperimentsHandler {
	h.metricsDBURL = metricsDBURL
	h.flavorNameFn = flavorNameFn
	h.capacityFreshness = freshness
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
	// Live capacity — which accelerator types actually have schedulable capacity right now,
	// per cluster. Complements the static catalog above: the catalog says what types exist and
	// their billing rate, this says what's actually available to land a job on at this moment.
	r.Get("/resource-catalog/capacity", h.getResourceCapacity)
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

// ClusterCapacity is one cluster's live, per-accelerator-type capacity — the same numbers the
// scheduler's own admission loop reads (metricsdb.LiveClusterAcceleratorCapacity /
// LiveClusterAcceleratorTotalCapacity), just translated from internal flavor names to the
// agent-facing AcceleratorType name and served as JSON.
type ClusterCapacity struct {
	ClusterName string                  `json:"cluster_name"`
	Accelerators []AcceleratorCapacity `json:"accelerators"`
}

// AcceleratorCapacity is one accelerator type's live capacity on one cluster. Available can be
// less than Total (some in use), equal to Total (all free), or absent from a report entirely —
// a type with no live report on a cluster within the freshness window is simply omitted, same
// "stale/disconnected contributes nothing" rule metricsdb.LiveClusterAcceleratorCapacity uses.
type AcceleratorCapacity struct {
	AcceleratorType string `json:"accelerator_type"`
	Available       int64  `json:"available"`
	Total           int64  `json:"total"`
}

// getResourceCapacity serves GET /resource-catalog/capacity: live, per-cluster, per-accelerator-type
// capacity, so an agent can check what's actually schedulable *right now* before submitting a job —
// GET /resource-catalog alone only says what types exist and their billing rate, not whether any
// cluster currently has one free. Types the operator never wired live-capacity reporting for
// (h.metricsDBURL unset) yield an empty list rather than an error — a documented "unknown" state,
// not a broken one.
func (h *PlatformExperimentsHandler) getResourceCapacity(w http.ResponseWriter, r *http.Request) {
	if h.metricsDBURL == "" {
		respondJSON(w, http.StatusOK, map[string]any{"clusters": []ClusterCapacity{}})
		return
	}
	ctx := r.Context()
	available, err := metricsdb.LiveClusterAcceleratorCapacity(ctx, h.metricsDBURL, h.capacityFreshness)
	if err != nil {
		h.logger.Error("get resource capacity: available", zap.Error(err))
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	total, err := metricsdb.LiveClusterAcceleratorTotalCapacity(ctx, h.metricsDBURL, h.capacityFreshness)
	if err != nil {
		h.logger.Error("get resource capacity: total", zap.Error(err))
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	clusterNames := make(map[string]bool, len(available))
	for cluster := range available {
		clusterNames[cluster] = true
	}
	for cluster := range total {
		clusterNames[cluster] = true
	}

	clusters := make([]ClusterCapacity, 0, len(clusterNames))
	for cluster := range clusterNames {
		flavorsSeen := make(map[string]bool)
		for flavor := range available[cluster] {
			flavorsSeen[flavor] = true
		}
		for flavor := range total[cluster] {
			flavorsSeen[flavor] = true
		}
		accelerators := make([]AcceleratorCapacity, 0, len(flavorsSeen))
		for flavor := range flavorsSeen {
			name := h.flavorNameFn(flavor)
			if name == "" {
				continue // unknown flavor (operator config changed since this was reported) — skip rather than leak the internal flavor string
			}
			accelerators = append(accelerators, AcceleratorCapacity{
				AcceleratorType: name,
				Available:       available[cluster][flavor],
				Total:           total[cluster][flavor],
			})
		}
		clusters = append(clusters, ClusterCapacity{ClusterName: cluster, Accelerators: accelerators})
	}
	respondJSON(w, http.StatusOK, map[string]any{"clusters": clusters})
}
