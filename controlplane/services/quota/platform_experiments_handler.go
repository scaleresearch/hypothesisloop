package quota

import (
	"context"
	"errors"
	"sort"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
)

// PlatformExperimentsHandler exposes the Platform Experiment lifecycle over
// HTTP. Routes are registered via RegisterHuma (see huma_api.go).
type PlatformExperimentsHandler struct {
	svc     *PlatformExperimentsService
	logger  *zap.Logger
	catalog ResourceCatalog

	// Live-capacity lookup (GET /resource-catalog/capacity) — set via WithLiveCapacity.
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
	AcceleratorTypes []AcceleratorTypeInfo `json:"accelerator_types"`
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

// ErrLiveCapacityUnconfigured means WithLiveCapacity was never called: GET /resource-catalog/capacity
// has no metrics DB to read live capacity from. Returning an empty cluster list here used to read
// exactly like "no cluster currently has this type free" — a legitimate, common answer — instead of
// "this deployment is misconfigured". A job submitted against a type that genuinely has no capacity
// queues forever without erroring (see the endpoint's own doc), so masking a config gap behind that
// same empty answer turned an operator mistake into the same silent starvation. Fail fast instead
// (important.md: "no fallbacks - one path or error, fail fast").
var ErrLiveCapacityUnconfigured = errors.New("resource-catalog/capacity: metrics DB URL not configured (WithLiveCapacity was never called)")

// liveCapacity reads live per-cluster accelerator capacity, or fails fast if this handler was
// never wired to a metrics DB.
func (h *PlatformExperimentsHandler) liveCapacity(ctx context.Context) ([]ClusterCapacity, error) {
	if h.metricsDBURL == "" {
		return nil, ErrLiveCapacityUnconfigured
	}
	available, total, err := metricsdb.LiveClusterAcceleratorAvailableAndTotal(ctx, h.metricsDBURL, h.capacityFreshness)
	if err != nil {
		return nil, err
	}
	return buildClusterCapacity(available, total), nil
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
