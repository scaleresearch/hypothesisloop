package platformexperiments

import (
	"context"
	"errors"
	"maps"
	"slices"
	"sort"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
)

// WithGitProxy is where the control plane's proxy serves repositories: a platform experiment's
// answer names its repository there, the address what the platform launches clones and pushes.
func (s *Service) WithGitProxy(proxyURL string) *Service {
	s.proxyURL = proxyURL
	return s
}

// WithPlanning sets what bounds a draft, from the settings file's planning section.
func (s *Service) WithPlanning(planning Planning) *Service {
	if planning.Limit <= 0 || planning.ProposedFor <= 0 || planning.PlannerKept <= 0 {
		panic("quota: WithPlanning requires every planning setting")
	}
	s.planning = planning
	return s
}

// ClusterRegistry says whose each registered cluster is and whether the platform shares it, and
// which org a project is of. *db.Store satisfies it.
type ClusterRegistry interface {
	RegisteredClusters(ctx context.Context) (map[string]domain.Cluster, error)
	ProjectOrg(ctx context.Context, projectID string) (string, error)
}

// WithLiveCapacity attaches what GET /resource-catalog/capacity needs: the freshness window past which a cluster's last
// report is treated as stale, and the registry that says which clusters a caller may see. The
// service checks a platform experiment against the same read.
func (s *Service) WithLiveCapacity(freshness time.Duration, clusters ClusterRegistry, podResources domain.PodResources) *Service {
	s.podResources = podResources
	s.capacityFreshness = freshness
	s.clusters = clusters
	s.reachable = s.reachableTypes
	s.placement = s.placementOf
	return s
}

// ClusterCapacity is one cluster's live capacity, per accelerator type.
type ClusterCapacity struct {
	ClusterName string `json:"cluster_name"`
	// OnDemand says the cluster rents a machine when a job needs one and has none standing by.
	// Its totals are then the most it rents at once, and what is available is what it may still
	// rent. The runtime says so itself, by reporting its nodes as slots (domain.NodeSlotLabel).
	OnDemand bool `json:"on_demand,omitempty"`
	// NoOffer is why the runtime of a cluster that rents on demand has no machine to rent for
	// some of its slots now, one line per reason as the runtime words it (domain.NodeSlotNoOfferLabel).
	NoOffer []string `json:"no_offer,omitempty" doc:"On a cluster that rents on demand: why it has no machine to rent now for some of its slots, as its runtime words it, such as which of the provider filters leave out every offer of a shape. A job that needs such a slot waits until one comes on offer."`
	// Resources is the cluster's cores and memory, which is all a job that asks for no
	// accelerator needs to know. Absent for a cluster that reported none in the window.
	Resources *ResourceCapacity `json:"resources,omitempty"`
	// PerNode is what one node of the cluster offers, which bounds what one job gets: a job's
	// cpu, memory and accelerator_count are per node. Absent for a cluster that reported no nodes.
	PerNode      *NodeCapacity         `json:"per_node,omitempty"`
	Accelerators []AcceleratorCapacity `json:"accelerators"`
}

// NodeCapacity holds, each figure on its own, the most any one node of a cluster has: installed
// cores and memory, and what running jobs leave free of them. A job asking for more cores or
// memory than CPUCores or MemoryBytes fits no node and never runs there.
type NodeCapacity struct {
	CPUCores             float64 `json:"cpu_cores_max"`
	MemoryBytes          int64   `json:"memory_bytes_max"`
	CPUCoresAvailable    float64 `json:"cpu_cores_available_max"`
	MemoryBytesAvailable int64   `json:"memory_bytes_available_max"`
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
	// Kind is what available and total count: devices, or for cpu cores.
	Kind      domain.TypeKind `json:"kind"`
	Available int64           `json:"available"`
	Total     int64           `json:"total"`
	// PerNodeAvailable is the most of the type free on any one node now: what a job's
	// accelerator_count can be and start without waiting for other jobs to end.
	PerNodeAvailable int64 `json:"per_node_available"`
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
func (s *Service) liveCapacity(ctx context.Context, orgID string, every bool) ([]ClusterCapacity, error) {
	if s.clusters == nil {
		return nil, ErrLiveCapacityUnconfigured
	}
	available, total, err := s.metrics.LiveClusterAcceleratorAvailableAndTotal(ctx, s.capacityFreshness)
	if err != nil {
		return nil, err
	}
	clusters := buildClusterCapacity(available, total, s.podResources)
	cpuAvailable, err := s.metrics.LiveClusterCPUCapacity(ctx, s.capacityFreshness)
	if err != nil {
		return nil, err
	}
	cpuTotal, err := s.metrics.LiveClusterCPUTotalCapacity(ctx, s.capacityFreshness)
	if err != nil {
		return nil, err
	}
	memoryAvailable, err := s.metrics.LiveClusterRAMCapacity(ctx, s.capacityFreshness)
	if err != nil {
		return nil, err
	}
	memoryTotal, err := s.metrics.LiveClusterRAMTotalCapacity(ctx, s.capacityFreshness)
	if err != nil {
		return nil, err
	}
	clusters = withResources(clusters, cpuAvailable, cpuTotal, memoryAvailable, memoryTotal)
	registered, err := s.clusters.RegisteredClusters(ctx)
	if err != nil {
		return nil, err
	}
	nodeLabels, err := s.metrics.LiveClusterNodeLabels(ctx, s.capacityFreshness)
	if err != nil {
		return nil, err
	}
	withOnDemand(clusters, nodeLabels)
	nodeTotal, err := s.metrics.LiveClusterNodeTotalCapacity(ctx, s.capacityFreshness)
	if err != nil {
		return nil, err
	}
	nodeFree, err := s.metrics.LiveClusterNodeResourceCapacity(ctx, s.capacityFreshness)
	if err != nil {
		return nil, err
	}
	nodeAcceleratorsFree, err := s.metrics.LiveClusterNodeAcceleratorCapacity(ctx, s.capacityFreshness)
	if err != nil {
		return nil, err
	}
	withPerNode(clusters, nodeTotal, nodeFree, nodeAcceleratorsFree)
	// Staff see every name that reports, registered or not: a runtime started under a name
	// nobody registered is theirs to notice.
	if every {
		return clusters, nil
	}
	return placeableBy(orgID, registered, clusters), nil
}

// reachableTypes are the accelerator types jobs of the org of projectID can be placed on now,
// each with the clusters that have it and the one image such a cluster runs, if it runs one only.
func (s *Service) reachableTypes(ctx context.Context, projectID string) (map[string][]domain.Cluster, error) {
	orgID, err := s.clusters.ProjectOrg(ctx, projectID)
	if err != nil {
		return nil, err
	}
	clusters, err := s.liveCapacity(ctx, orgID, false)
	if err != nil {
		return nil, err
	}
	return typesListed(clusters), nil
}

// typesListed is every type line of a capacity answer, with the clusters that have it: the types `hl capacity` prints are exactly the ones a proposal
// may be sized for.
func typesListed(clusters []ClusterCapacity) map[string][]domain.Cluster {
	listed := map[string][]domain.Cluster{}
	for _, cluster := range clusters {
		for _, accelerator := range cluster.Accelerators {
			listed[accelerator.AcceleratorType] = append(listed[accelerator.AcceleratorType], domain.Cluster{Name: cluster.ClusterName})
		}
	}
	return listed
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

// withOnDemand marks the clusters whose nodes are slots, each a machine the runtime may rent, and
// says why the runtime has nothing to rent for the slots it has none for.
func withOnDemand(clusters []ClusterCapacity, nodeLabels map[string]map[string]map[string]string) {
	for i := range clusters {
		noOffer := map[string]bool{}
		for _, labels := range nodeLabels[clusters[i].ClusterName] {
			if labels[domain.NodeSlotLabel] != "" {
				clusters[i].OnDemand = true
			}
			if why := labels[domain.NodeSlotNoOfferLabel]; why != "" {
				noOffer[why] = true
			}
		}
		clusters[i].NoOffer = slices.Sorted(maps.Keys(noOffer))
	}
}

// withPerNode says of each cluster the most one node has, in total and free, of cores and memory,
// and on each type's line the most of it free on one node. The maps are cluster -> node -> key.
func withPerNode(clusters []ClusterCapacity, nodeTotal, nodeFree, nodeAcceleratorsFree map[string]map[string]map[string]int64) {
	for i := range clusters {
		cluster := &clusters[i]
		total, free := nodeTotal[cluster.ClusterName], nodeFree[cluster.ClusterName]
		if len(total) > 0 || len(free) > 0 {
			perNode := NodeCapacity{}
			for _, resources := range total {
				perNode.CPUCores = max(perNode.CPUCores, float64(resources[domain.NodeResourceCPUMillicores])/1000)
				perNode.MemoryBytes = max(perNode.MemoryBytes, resources[domain.NodeResourceMemoryBytes])
			}
			for _, resources := range free {
				perNode.CPUCoresAvailable = max(perNode.CPUCoresAvailable, float64(resources[domain.NodeResourceCPUMillicores])/1000)
				perNode.MemoryBytesAvailable = max(perNode.MemoryBytesAvailable, resources[domain.NodeResourceMemoryBytes])
			}
			cluster.PerNode = &perNode
		}
		for a := range cluster.Accelerators {
			line := &cluster.Accelerators[a]
			for _, byType := range nodeAcceleratorsFree[cluster.ClusterName] {
				line.PerNodeAvailable = max(line.PerNodeAvailable, byType[line.AcceleratorType])
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
func buildClusterCapacity(available, total map[string]map[string]int64, podResources domain.PodResources) []ClusterCapacity {
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
			// A type the cluster has none of places no job, so it is no line to choose from.
			if total[cluster][t] <= 0 {
				continue
			}
			rate, _ := domain.LookupAcceleratorCost(domain.AcceleratorType(t))
			accelerators = append(accelerators, AcceleratorCapacity{
				AcceleratorType: t,
				Kind:            domain.AcceleratorType(t).Kind(),
				Available:       available[cluster][t],
				Total:           total[cluster][t],
				AccHPerHour:     rate,
				PodResources:    podResources.Of(domain.AcceleratorType(t)),
			})
		}
		sort.Slice(accelerators, func(i, j int) bool { return accelerators[i].AcceleratorType < accelerators[j].AcceleratorType })
		clusters = append(clusters, ClusterCapacity{ClusterName: cluster, Accelerators: accelerators})
	}
	sort.Slice(clusters, func(i, j int) bool { return clusters[i].ClusterName < clusters[j].ClusterName })
	return clusters
}

// WithAgentLaunch attaches what joining needs beyond the store.
func (s *Service) WithAgentLaunch(launch AgentLaunch) *Service {
	s.launch = launch
	return s
}

// refusal answers a refusal as it is; anything else is the platform's own failure, logged and
// answered without detail.
func (s *Service) refusal(op string, err error) error {
	var refused *refusal.Refusal
	if errors.As(err, &refused) {
		return refused
	}
	return auth.Internal(s.logger, op+" failed", err)
}

// ResourceCapacity is the live capacity of the clusters orgID's jobs can be placed on, or of
// every cluster with every.
func (s *Service) ResourceCapacity(ctx context.Context, orgID string, every bool) ([]ClusterCapacity, error) {
	return s.liveCapacity(ctx, orgID, every)
}
