package quota

import (
	"sort"
	"time"

	"go.uber.org/zap"
)

// PlatformExperimentsHandler exposes the Platform Experiment lifecycle over
// HTTP. Routes are registered via RegisterHuma (see huma_api.go).
type PlatformExperimentsHandler struct {
	svc     *PlatformExperimentsService
	logger  *zap.Logger
	catalog ResourceCatalog

	// Live-capacity lookup (GET /resource-catalog/capacity) — unset metricsDBURL means an
	// empty capacity list instead of a panic.
	metricsDBURL      string
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
// per-flavor capacity reports land in, and the freshness window past which a cluster's last
// report is treated as stale.
func (h *PlatformExperimentsHandler) WithLiveCapacity(metricsDBURL string, freshness time.Duration) *PlatformExperimentsHandler {
	h.metricsDBURL = metricsDBURL
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

// ClusterCapacity is one cluster's live capacity, per accelerator type.
type ClusterCapacity struct {
	ClusterName  string                `json:"cluster_name"`
	Accelerators []AcceleratorCapacity `json:"accelerators"`
}

// AcceleratorCapacity is one accelerator type's live capacity on one cluster. AcceleratorType is
// the driver-published "key=value" string — exactly what a job spec's accelerator_type takes, so
// an agent reads this and submits the same string with nothing in between to get wrong.
type AcceleratorCapacity struct {
	AcceleratorType string `json:"accelerator_type"`
	Available       int64  `json:"available"`
	Total           int64  `json:"total"`
}

// buildClusterCapacity assembles the agent-facing view from live per-cluster/per-type counts.
// Both maps are already keyed by accelerator type, so there is nothing to translate here.
func buildClusterCapacity(available, total map[string]map[string]int64) []ClusterCapacity {
	clusterNames := make(map[string]bool, len(available))
	for cluster := range available {
		clusterNames[cluster] = true
	}
	for cluster := range total {
		clusterNames[cluster] = true
	}
	clusters := make([]ClusterCapacity, 0, len(clusterNames))
	for cluster := range clusterNames {
		typesSeen := make(map[string]bool)
		for t := range available[cluster] {
			typesSeen[t] = true
		}
		for t := range total[cluster] {
			typesSeen[t] = true
		}
		accelerators := make([]AcceleratorCapacity, 0, len(typesSeen))
		for t := range typesSeen {
			accelerators = append(accelerators, AcceleratorCapacity{
				AcceleratorType: t,
				Available:       available[cluster][t],
				Total:           total[cluster][t],
			})
		}
		sort.Slice(accelerators, func(i, j int) bool { return accelerators[i].AcceleratorType < accelerators[j].AcceleratorType })
		clusters = append(clusters, ClusterCapacity{ClusterName: cluster, Accelerators: accelerators})
	}
	sort.Slice(clusters, func(i, j int) bool { return clusters[i].ClusterName < clusters[j].ClusterName })
	return clusters
}
