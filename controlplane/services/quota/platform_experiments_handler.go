package quota

import (
	"time"

	"go.uber.org/zap"
)

// PlatformExperimentsHandler exposes the Platform Experiment lifecycle over
// HTTP. Routes are registered via RegisterHuma (see huma_api.go).
type PlatformExperimentsHandler struct {
	svc     *PlatformExperimentsService
	logger  *zap.Logger
	catalog ResourceCatalog

	// Live-capacity lookup (GET /resource-catalog/capacity) — an unset metricsDBURL means the
	// endpoint responds with an empty capacity list rather than panicking.
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

// WithLiveCapacity attaches what GET /resource-catalog/capacity needs: the metrics DB URL
// live per-flavor accelerator capacity reports land in, a flavor->accelerator-type-name
// translator, and the freshness window past which a cluster's last report is treated as stale.
func (h *PlatformExperimentsHandler) WithLiveCapacity(metricsDBURL string, flavorNameFn func(flavor string) string, freshness time.Duration) *PlatformExperimentsHandler {
	h.metricsDBURL = metricsDBURL
	h.flavorNameFn = flavorNameFn
	h.capacityFreshness = freshness
	return h
}

// AcceleratorTypeInfo is the agent/UI-facing view of one catalog accelerator type — pricing only.
type AcceleratorTypeInfo struct {
	Name     string  `json:"name"`
	AccHRate float64 `json:"acch_rate"`
}

// ResourceCatalog is the full set of resource-pricing reference data served by GET /resource-catalog.
type ResourceCatalog struct {
	AcceleratorTypes  []AcceleratorTypeInfo `json:"accelerator_types"`
	CPUCoreHourRate   float64               `json:"cpu_core_hour_rate"`
	RAMGBHourRate     float64               `json:"ram_gb_hour_rate"`
	StorageGBHourRate float64               `json:"storage_gb_hour_rate"`
}

// ClusterCapacity is one cluster's live, per-accelerator-type capacity.
type ClusterCapacity struct {
	ClusterName  string                `json:"cluster_name"`
	Accelerators []AcceleratorCapacity `json:"accelerators"`
}

// AcceleratorCapacity is one accelerator type's live capacity on one cluster.
type AcceleratorCapacity struct {
	AcceleratorType string `json:"accelerator_type"`
	Available       int64  `json:"available"`
	Total           int64  `json:"total"`
}

// buildClusterCapacity translates the raw per-cluster/per-flavor available and total maps
// (internal flavor names) into the agent-facing ClusterCapacity list (accelerator-type names).
// Unknown flavors (operator config changed since a report) are skipped rather than leaked.
func buildClusterCapacity(available, total map[string]map[string]int64, flavorNameFn func(string) string) []ClusterCapacity {
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
			name := flavorNameFn(flavor)
			if name == "" {
				continue
			}
			accelerators = append(accelerators, AcceleratorCapacity{
				AcceleratorType: name,
				Available:       available[cluster][flavor],
				Total:           total[cluster][flavor],
			})
		}
		clusters = append(clusters, ClusterCapacity{ClusterName: cluster, Accelerators: accelerators})
	}
	return clusters
}
