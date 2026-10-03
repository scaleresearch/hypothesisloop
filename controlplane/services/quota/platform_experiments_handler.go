package quota

import (
	"context"
	"errors"
	"sort"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/githost"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
)

// PlatformExperimentsHandler exposes the Platform Experiment lifecycle over
// HTTP. Routes are registered via RegisterHuma (see huma_api.go).
type PlatformExperimentsHandler struct {
	svc    *PlatformExperimentsService
	logger *zap.Logger
	// jobImages is the images the deployment offers a job to run in.
	jobImages []domain.JobImage

	// Live-capacity lookup (GET /resource-catalog/capacity) — set via WithLiveCapacity.
	metricsDBURL      string
	capacityFreshness time.Duration
	clusters          ClusterRegistry

	// Set via WithAgentLaunch.
	launch AgentLaunch
	// What bounds a draft, from the settings file's planning section.
	planning Planning
	// repos is the git host platform experiments keep their code on.
	repos githost.Provider
}

// NewPlatformExperimentsHandler constructs the handler.
func NewPlatformExperimentsHandler(svc *PlatformExperimentsService, repos githost.Provider, planning Planning, logger *zap.Logger) *PlatformExperimentsHandler {
	if planning.Limit <= 0 || planning.ProposedFor <= 0 {
		panic("quota: NewPlatformExperimentsHandler requires every planning setting")
	}
	return &PlatformExperimentsHandler{svc: svc, repos: repos, logger: logger, planning: planning}
}

// WithJobImages attaches the images the deployment offers a job to run in.
func (h *PlatformExperimentsHandler) WithJobImages(images []domain.JobImage) *PlatformExperimentsHandler {
	h.jobImages = images
	return h
}

// ClusterRegistry says whose each registered cluster is and whether the platform shares it, and
// which org a project is of. *db.Store satisfies it.
type ClusterRegistry interface {
	RegisteredClusters(ctx context.Context) (map[string]domain.Cluster, error)
	ProjectOrg(ctx context.Context, projectID string) (string, error)
}

// WithLiveCapacity attaches what GET /resource-catalog/capacity needs: the metrics DB URL
// per-flavor capacity reports land in, the freshness window past which a cluster's last
// report is treated as stale, and the registry that says which clusters a caller may see. The
// service checks a platform experiment against the same read.
func (h *PlatformExperimentsHandler) WithLiveCapacity(metricsDBURL string, freshness time.Duration, clusters ClusterRegistry) *PlatformExperimentsHandler {
	h.metricsDBURL = metricsDBURL
	h.capacityFreshness = freshness
	h.clusters = clusters
	h.svc.reachable = h.reachableTypes
	return h
}

// ClusterCapacity is one cluster's live capacity, per accelerator type.
type ClusterCapacity struct {
	ClusterName string `json:"cluster_name"`
	// JobImage is the one image the cluster runs: a job placed there names none or it. Absent,
	// the cluster takes only jobs that name their image.
	JobImage string `json:"job_image,omitempty"`
	// JobImageCarries is what is installed in JobImage, when it is an image the deployment offers.
	JobImageCarries string `json:"job_image_carries,omitempty"`
	// OnDemand says the cluster rents a machine when a job needs one and has none standing by.
	// Its totals are then the most it rents at once, and what is available is what it may still
	// rent. The runtime says so itself, by reporting its nodes as slots (domain.NodeSlotLabel).
	OnDemand bool `json:"on_demand,omitempty"`
	// Resources is the cluster's cores and memory, which is all a job that asks for no
	// accelerator needs to know. Absent for a cluster that reported none in the window.
	Resources    *ResourceCapacity     `json:"resources,omitempty"`
	Accelerators []AcceleratorCapacity `json:"accelerators"`
}

// ResourceCapacity is a cluster's cores and memory as its runtime last reported them: what its
// nodes offer in total and what running jobs have not asked for. Both sum over the nodes, so
// they bound what the cluster runs at once and say nothing of what fits on one node.
type ResourceCapacity struct {
	CPUCoresAvailable    float64 `json:"cpu_cores_available"`
	CPUCoresTotal        float64 `json:"cpu_cores_total"`
	MemoryBytesAvailable int64   `json:"memory_bytes_available"`
	MemoryBytesTotal     int64   `json:"memory_bytes_total"`
}

// AcceleratorCapacity is one accelerator type's live capacity on one cluster. AcceleratorType is
// the driver-published "key=value" string — exactly what a job spec's accelerator_type takes, so
// an agent reads this and submits the same string with nothing in between to get wrong.
type AcceleratorCapacity struct {
	AcceleratorType string `json:"accelerator_type"`
	// JobImages names the offered images that suit this type (see the answer's job_images).
	JobImages []string `json:"job_images,omitempty"`
	// Kind is what available and total count: devices, or for cpu cores.
	Kind      domain.TypeKind `json:"kind"`
	Available int64           `json:"available"`
	Total     int64           `json:"total"`
	// AccHPerHour is what one accelerator of this type costs per hour of run, in accelerator
	// hours: the factor between a job's run time and its cost.
	AccHPerHour float64 `json:"acch_per_hour,omitempty"`
	// PodResources is what the platform adds to every node of a job on this type beside the
	// device, as the catalog states it (accelerator_types[].pod_resources).
	PodResources map[string]string `json:"pod_resources,omitempty" doc:"What the platform adds to every node of a job on this type, by Kubernetes resource name, such as the hugepages a Tenstorrent Blackhole chip's driver maps. A job states none of it: it is added over the job's own accelerator_pod_resources."`
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
	cpuAvailable, err := metricsdb.LiveClusterCPUCapacity(ctx, h.metricsDBURL, h.capacityFreshness)
	if err != nil {
		return nil, err
	}
	cpuTotal, err := metricsdb.LiveClusterCPUTotalCapacity(ctx, h.metricsDBURL, h.capacityFreshness)
	if err != nil {
		return nil, err
	}
	memoryAvailable, err := metricsdb.LiveClusterRAMCapacity(ctx, h.metricsDBURL, h.capacityFreshness)
	if err != nil {
		return nil, err
	}
	memoryTotal, err := metricsdb.LiveClusterRAMTotalCapacity(ctx, h.metricsDBURL, h.capacityFreshness)
	if err != nil {
		return nil, err
	}
	clusters = withResources(clusters, cpuAvailable, cpuTotal, memoryAvailable, memoryTotal)
	registered, err := h.clusters.RegisteredClusters(ctx)
	if err != nil {
		return nil, err
	}
	withJobImages(registered, clusters)
	withOfferedImages(h.jobImages, clusters)
	nodeLabels, err := metricsdb.LiveClusterNodeLabels(ctx, h.metricsDBURL, h.capacityFreshness)
	if err != nil {
		return nil, err
	}
	withOnDemand(clusters, nodeLabels)
	// Staff see every name that reports, registered or not: a runtime started under a name
	// nobody registered is theirs to notice.
	if every {
		return clusters, nil
	}
	return placeableBy(orgID, registered, clusters), nil
}

// reachableTypes are the accelerator types jobs of the org of projectID can be placed on now,
// each with the clusters that have it and the one image such a cluster runs, if it runs one only.
func (h *PlatformExperimentsHandler) reachableTypes(ctx context.Context, projectID string) (map[string][]domain.Cluster, error) {
	orgID, err := h.clusters.ProjectOrg(ctx, projectID)
	if err != nil {
		return nil, err
	}
	clusters, err := h.liveCapacity(ctx, orgID, false)
	if err != nil {
		return nil, err
	}
	reachable := map[string][]domain.Cluster{}
	for _, cluster := range clusters {
		for _, accelerator := range cluster.Accelerators {
			if accelerator.Total > 0 {
				reachable[accelerator.AcceleratorType] = append(reachable[accelerator.AcceleratorType], domain.Cluster{Name: cluster.ClusterName, JobImage: cluster.JobImage})
			}
		}
	}
	return reachable, nil
}

// withResources adds each cluster's cores and memory, and the clusters that have no accelerator
// at all: a job that asks for none can run there, and they were in no answer before.
func withResources(clusters []ClusterCapacity, cpuAvailable, cpuTotal map[string]float64, memoryAvailable, memoryTotal map[string]int64) []ClusterCapacity {
	listed := make(map[string]bool, len(clusters))
	for _, cluster := range clusters {
		listed[cluster.ClusterName] = true
	}
	for name := range cpuTotal {
		if !listed[name] {
			clusters = append(clusters, ClusterCapacity{ClusterName: name, Accelerators: []AcceleratorCapacity{}})
		}
	}
	for i, cluster := range clusters {
		total, reported := cpuTotal[cluster.ClusterName]
		if !reported {
			continue
		}
		clusters[i].Resources = &ResourceCapacity{
			CPUCoresAvailable: cpuAvailable[cluster.ClusterName], CPUCoresTotal: total,
			MemoryBytesAvailable: memoryAvailable[cluster.ClusterName], MemoryBytesTotal: memoryTotal[cluster.ClusterName],
		}
	}
	sort.Slice(clusters, func(i, j int) bool { return clusters[i].ClusterName < clusters[j].ClusterName })
	return clusters
}

// withOfferedImages names on each capacity line the offered images that suit its type, and says
// what a cluster's own job image carries when it is one of them. A cluster that runs its own
// image and no other gets no names: a job there can name none of them.
func withOfferedImages(offered []domain.JobImage, clusters []ClusterCapacity) {
	for c := range clusters {
		cluster := &clusters[c]
		for _, image := range offered {
			if cluster.JobImage != "" && image.Image == cluster.JobImage {
				cluster.JobImageCarries = image.Carries
			}
		}
		if cluster.JobImage != "" {
			continue
		}
		for a := range cluster.Accelerators {
			for _, image := range offered {
				if image.SuitsType(cluster.Accelerators[a].AcceleratorType) {
					cluster.Accelerators[a].JobImages = append(cluster.Accelerators[a].JobImages, image.Name)
				}
			}
		}
	}
}

// withJobImages says of each cluster the one image it runs, if it runs one only, so that a
// submitter reads from the same answer what to ask for and whether to name an image.
func withJobImages(registered map[string]domain.Cluster, clusters []ClusterCapacity) {
	for i := range clusters {
		clusters[i].JobImage = registered[clusters[i].ClusterName].JobImage
	}
}

// withOnDemand marks the clusters whose nodes are slots, each a machine the runtime may rent.
func withOnDemand(clusters []ClusterCapacity, nodeLabels map[string]map[string]map[string]string) {
	for i := range clusters {
		for _, labels := range nodeLabels[clusters[i].ClusterName] {
			if labels[domain.NodeSlotLabel] != "" {
				clusters[i].OnDemand = true
				break
			}
		}
	}
}

// placeableBy keeps the clusters that take jobs of orgID: its own and the shared ones that it has
// not turned off, the rule admission places by (domain.Cluster.Hosts). Capacity on a cluster the
// org turned off is not capacity its jobs can get, so it is left out. A name nobody registered is
// no cluster.
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
				Kind:            domain.AcceleratorType(t).Kind(),
				Available:       available[cluster][t],
				Total:           total[cluster][t],
				AccHPerHour:     rate,
				PodResources:    domain.AcceleratorType(t).PodResources(),
			})
		}
		sort.Slice(accelerators, func(i, j int) bool { return accelerators[i].AcceleratorType < accelerators[j].AcceleratorType })
		clusters = append(clusters, ClusterCapacity{ClusterName: cluster, Accelerators: accelerators})
	}
	sort.Slice(clusters, func(i, j int) bool { return clusters[i].ClusterName < clusters[j].ClusterName })
	return clusters
}
