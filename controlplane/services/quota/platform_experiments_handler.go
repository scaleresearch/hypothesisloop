package quota

import (
	"context"
	"errors"
	"sort"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
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
	clusters          ClusterRegistry

	// Set via WithAgentLaunch.
	launch AgentLaunch

	// repos reaches the repositories a project's org connected on GitHub.
	repos Repositories
}

// NewPlatformExperimentsHandler constructs the handler.
func NewPlatformExperimentsHandler(svc *PlatformExperimentsService, repos Repositories, logger *zap.Logger) *PlatformExperimentsHandler {
	return &PlatformExperimentsHandler{svc: svc, repos: repos, logger: logger}
}

// WithCatalog attaches the resource catalog served by GET /resource-catalog.
func (h *PlatformExperimentsHandler) WithCatalog(catalog ResourceCatalog) *PlatformExperimentsHandler {
	h.catalog = catalog
	return h
}

// ClusterRegistry says whose each registered cluster is and whether the platform shares it.
// *db.ClustersStore satisfies it.
type ClusterRegistry interface {
	RegisteredClusters(ctx context.Context) (map[string]domain.Cluster, error)
}

// WithLiveCapacity attaches what GET /resource-catalog/capacity needs: the metrics DB URL
// per-flavor capacity reports land in, the freshness window past which a cluster's last
// report is treated as stale, and the registry that says which clusters a caller may see.
func (h *PlatformExperimentsHandler) WithLiveCapacity(metricsDBURL string, freshness time.Duration, clusters ClusterRegistry) *PlatformExperimentsHandler {
	h.metricsDBURL = metricsDBURL
	h.capacityFreshness = freshness
	h.clusters = clusters
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
	// AccHPerHour is what one accelerator of this type costs per hour of run, in accelerator
	// hours: the factor between a job's run time and its cost.
	AccHPerHour float64 `json:"acch_per_hour,omitempty"`
}

// ErrLiveCapacityUnconfigured means WithLiveCapacity was never called: GET /resource-catalog/capacity
// has no metrics DB to read live capacity from. Returning an empty cluster list here used to read
// exactly like "no cluster currently has this type free" — a legitimate, common answer — instead of
// "this deployment is misconfigured". A job submitted against a type that genuinely has no capacity
// queues forever without erroring (see the endpoint's own doc), so masking a config gap behind that
// same empty answer turned an operator mistake into the same silent starvation. Fail fast instead
// (important.md: "no fallbacks - one path or error, fail fast").
var ErrLiveCapacityUnconfigured = errors.New("resource-catalog/capacity: metrics DB URL not configured (WithLiveCapacity was never called)")

// liveCapacity reads the live accelerator capacity of the clusters jobs of orgID can be placed
// on, or of every cluster that reports when every is set, which is staff's view. It fails fast
// if this handler was never wired to a metrics DB.
func (h *PlatformExperimentsHandler) liveCapacity(ctx context.Context, orgID string, every bool) ([]ClusterCapacity, error) {
	if h.metricsDBURL == "" {
		return nil, ErrLiveCapacityUnconfigured
	}
	available, total, err := metricsdb.LiveClusterAcceleratorAvailableAndTotal(ctx, h.metricsDBURL, h.capacityFreshness)
	if err != nil {
		return nil, err
	}
	clusters := buildClusterCapacity(available, total)
	// Staff see every name that reports, registered or not: a runtime started under a name
	// nobody registered is theirs to notice.
	if every {
		return clusters, nil
	}
	registered, err := h.clusters.RegisteredClusters(ctx)
	if err != nil {
		return nil, err
	}
	return placeableBy(orgID, registered, clusters), nil
}

// placeableBy keeps the clusters that take jobs of orgID: its own and the shared ones, the rule
// admission places by (domain.Cluster.Hosts). A name nobody registered is no cluster.
func placeableBy(orgID string, registered map[string]domain.Cluster, clusters []ClusterCapacity) []ClusterCapacity {
	kept := make([]ClusterCapacity, 0, len(clusters))
	for _, c := range clusters {
		if cluster, ok := registered[c.ClusterName]; ok && cluster.Hosts(orgID) {
			kept = append(kept, c)
		}
	}
	return kept
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
			rate, _ := domain.AcceleratorType(t).LookupCost()
			accelerators = append(accelerators, AcceleratorCapacity{
				AcceleratorType: t,
				Available:       available[cluster][t],
				Total:           total[cluster][t],
				AccHPerHour:     rate,
			})
		}
		sort.Slice(accelerators, func(i, j int) bool { return accelerators[i].AcceleratorType < accelerators[j].AcceleratorType })
		clusters = append(clusters, ClusterCapacity{ClusterName: cluster, Accelerators: accelerators})
	}
	sort.Slice(clusters, func(i, j int) bool { return clusters[i].ClusterName < clusters[j].ClusterName })
	return clusters
}
