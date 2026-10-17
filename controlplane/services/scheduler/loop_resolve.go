package scheduler

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/services/scheduler/placement"
)

// resolutionCache amortizes the running-experiment node-attribution lookups the per-accelerator
// share needs (see resolveClusterLocalResources) across every queued job considered in one
// tick: the running set and each running job's node are the same answer for every queued job
// asking about a given accelerator flavor, so this is built lazily and reused rather than
// re-queried once per queued job. The speculative-candidate reads are held the same way.
type resolutionCache struct {
	l               *Loop
	claimed         []*domain.Experiment // RUNNING and SUBMITTED — see loadClaimed
	claimedLoaded   bool
	nodeOf          map[string]string // experiment ID -> node, filled lazily
	recentlyTried   map[string]bool   // cluster IDs any job failed over from within the TTL
	speculativeCaps map[string]*int   // cluster ID -> max_speculative_accelerators, nil for no cap
}

func newResolutionCache(l *Loop) *resolutionCache {
	return &resolutionCache{l: l, nodeOf: map[string]string{}, speculativeCaps: map[string]*int{}}
}

func (c *resolutionCache) recentlyTriedClusters(ctx context.Context) (map[string]bool, error) {
	if c.recentlyTried == nil {
		tried, err := c.l.store.RecentlyTriedClusters(ctx, c.l.triedClusterTTL)
		if err != nil {
			return nil, err
		}
		c.recentlyTried = tried
	}
	return c.recentlyTried, nil
}

func (c *resolutionCache) speculativeCap(ctx context.Context, clusterID string) (*int, error) {
	if cap, ok := c.speculativeCaps[clusterID]; ok {
		return cap, nil
	}
	cap, err := c.l.clusterSpeculativeCap(ctx, clusterID)
	if err != nil {
		return nil, err
	}
	c.speculativeCaps[clusterID] = cap
	return cap, nil
}

// loadClaimed returns every experiment that has already claimed accelerator capacity toward the
// nodes it landed on — RUNNING and SUBMITTED, not just RUNNING. A RUNNING-only view
// undercounts: SUBMITTED jobs have already reduced nodeAvail's free count but have not
// yet reached RUNNING, so leaving them out of "installed" here would corrupt every FairShare
// computed against the reconstructed total.
func (c *resolutionCache) loadClaimed(ctx context.Context) ([]*domain.Experiment, error) {
	if c.claimedLoaded {
		return c.claimed, nil
	}
	claimed, err := c.l.store.ListCapacityClaimedExperiments(ctx)
	if err != nil {
		return nil, err
	}
	c.claimed = claimed
	c.claimedLoaded = true
	return claimed, nil
}

// installedAcceleratorsByNode returns, for every node cluster's nodeAvail reports, its installed
// (not free) count of flavor: currently free (nodeAvail) plus what is already claimed there by a
// RUNNING or SUBMITTED experiment (see loadClaimed — SUBMITTED jobs have
// already reduced nodeAvail's free count and must be counted here too, or "installed" undercounts
// and every FairShare computed against it is wrong), attributed the same way loop_disbalance.go's
// disbalance evictor attributes running work to a node — one metrics-store node-attribution query
// per claimed experiment of this flavor, cached across the whole tick (see resolutionCache's doc
// comment), not per queued job.
//
// Node attribution (metricsdb.LatestExperimentNode) is keyed by observed placement, so a
// SUBMITTED job the backend has not yet scheduled anywhere may not resolve to a node yet
// (found=false below) and is simply not counted at that node until it does — same as it would be
// missed entirely under the old RUNNING-only view, never worse.
//
// A claimed experiment spanning more than one node (r.Job.Nodes() > 1: a distributed or grouped
// job) is deliberately EXCLUDED from this reconstruction rather than attributed whole to its one
// "latest node". metricsdb only records ONE node per experiment (see LatestExperimentNode's doc
// comment — the metric is keyed by experiment_id alone, with no per-group/per-replica dimension),
// so there is no way to know which of that job's several nodes actually holds which share of its
// AcceleratorCount. Charging the whole total to the single reported node would overcount that one
// node and (implicitly, by never appearing at all) undercount every other node the job is really
// spread across — worse than the conservative undercount this exclusion accepts instead. This
// only affects the reconstructed INSTALLED total on the node metrics attributed the multi-node job
// to, while resolving a DIFFERENT job's fair share there — nodeAvail's live free-capacity report
// already reflects the real device usage on every node regardless of this exclusion.
//
// The cores of domain.CPUType are counted from the node's own total instead. Every job holds
// cores, an accelerator job too, so free cores plus the cores of CPU jobs falls short of what is
// installed by whatever the accelerator jobs hold, and a share measured against that would
// allow the CPU jobs memory the accelerator jobs already have.
func (c *resolutionCache) installedAcceleratorsByNode(ctx context.Context, cluster string, flavor domain.AcceleratorType, nodeAvail, nodeResourcesTotal map[string]map[string]int64) (map[string]int64, error) {
	if flavor.Kind() == domain.TypeKindCPU {
		return installedCoresByNode(nodeAvail, nodeResourcesTotal), nil
	}
	key := placement.AcceleratorKey(flavor)
	installed := make(map[string]int64, len(nodeAvail))
	for node, byFlavor := range nodeAvail {
		installed[node] = byFlavor[key]
	}
	claimed, err := c.loadClaimed(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve cluster-local resources: list capacity-claimed experiments: %w", err)
	}
	for _, r := range claimed {
		if r.ClusterName != cluster || r.AcceleratorCount <= 0 {
			continue
		}
		if !r.AcceleratorType.Is(flavor) {
			continue
		}
		if r.Job.Nodes() > 1 {
			continue // see the doc comment above: only a single-node claim is safe to attribute
		}
		node, ok := c.nodeOf[r.ID]
		if !ok {
			found := false
			node, found, err = c.l.observed.LatestExperimentNode(ctx, r.ID, r.CreatedAt, time.Now().UTC())
			if err != nil {
				return nil, fmt.Errorf("resolve cluster-local resources: node attribution for %s: %w", r.ID, err)
			}
			if !found {
				continue
			}
			c.nodeOf[r.ID] = node
		}
		if node == "" {
			continue
		}
		installed[node] += int64(r.AcceleratorCount)
	}
	return installed, nil
}

// installedCoresByNode is the whole cores of every node that offers them as domain.CPUType: the
// node's total where it reports one, what it has free otherwise.
func installedCoresByNode(nodeAvail, nodeResourcesTotal map[string]map[string]int64) map[string]int64 {
	installed := make(map[string]int64, len(nodeAvail))
	for node, byFlavor := range nodeAvail {
		for flavor, free := range byFlavor {
			if !domain.CPUType.Is(domain.AcceleratorType(flavor)) {
				continue
			}
			installed[node] = free
			if total := nodeResourcesTotal[node][domain.NodeResourceCPUMillicores] / 1000; total > 0 {
				installed[node] = total
			}
		}
	}
	return installed
}

// resolveClusterLocalResources returns exp's job as it runs on cluster, once cluster and
// accelerator flavor are decided for this attempt. A job states its cpu, memory and storage, so
// on a cluster of nodes that is the job as submitted, with each quantity checked against the
// job's cluster-local share: the MINIMUM domain.FairShare across every node in cluster that
// reports exp's accelerator flavor and carries exp.Job's node_selector.
//
// The check is PER GROUP: each group's share is computed against that group's own per-node
// accelerator count, never the job's TotalAccelerators. A group that holds no accelerator has no
// share and is not checked, and neither is a group no node of the cluster can bound yet (a
// speculative target with no live node): the capacity check of a later tick catches a request
// that is too large there.
//
// misfit is "" when the job's resources work at this cluster this tick, otherwise the
// not_admitted_reason to record, exceeds_accelerator_share. The caller un-picks the cluster
// exactly like a scalar or topology miss; a node with more cpu, memory or storage per accelerator
// may still admit the job later, so this is never a hard rejection.
//
// On a cluster of slots (domain.NodeSlotLabel) none of the above applies: the job is sized to the
// slot it is to hold, see placement.ResolveOntoSlot. nodeResources is what each node has free now, which
// only that case reads.
func (l *Loop) resolveClusterLocalResources(ctx context.Context, cache *resolutionCache, exp *domain.Experiment, cluster string, nodeResourcesTotal, nodeAvail, nodeResources map[string]map[string]int64, nodeLabels map[string]map[string]string) (resolved *domain.JobSpec, misfit string, err error) {
	if placement.HasSlots(nodeLabels) {
		resolved, misfit = placement.ResolveOntoSlot(exp, cluster, nodeResourcesTotal, nodeResources, nodeLabels)
		return resolved, misfit, nil
	}
	out := exp.Job
	groups := exp.Job.NodeGroups()
	if !slices.ContainsFunc(groups, func(g domain.JobGroup) bool { return g.AcceleratorCount > 0 }) {
		return &out, "", nil
	}

	installed, ierr := cache.installedAcceleratorsByNode(ctx, cluster, exp.AcceleratorType, nodeAvail, nodeResourcesTotal)
	if ierr != nil {
		return nil, "", ierr
	}

	for _, g := range groups {
		if g.AcceleratorCount <= 0 {
			continue
		}
		for _, dimension := range []struct{ name, key, requested string }{
			{"cpu", domain.NodeResourceCPUMillicores, g.CPU},
			{"memory", domain.NodeResourceMemoryBytes, g.Memory},
			{"storage", domain.NodeResourceStorageBytes, g.Storage},
		} {
			share, bounded := int64(0), false
			for node, total := range nodeResourcesTotal {
				if !placement.LabelsMatch(nodeLabels[node], exp.Job.NodeSelector) || installed[node] <= 0 {
					continue
				}
				ofNode, err := domain.FairShare(total[dimension.key], int64(g.AcceleratorCount), installed[node])
				if err != nil {
					return nil, "", fmt.Errorf("%s fair share: %w", dimension.name, err)
				}
				if !bounded || ofNode < share {
					share, bounded = ofNode, true
				}
			}
			if !bounded {
				continue
			}
			requested, err := placement.QuantityValue(dimension.key, dimension.requested)
			if err != nil {
				return nil, "", fmt.Errorf("job %s %q: %w", dimension.name, dimension.requested, err)
			}
			if requested > float64(share) {
				unit := "accelerator"
				if exp.AcceleratorType.Kind() == domain.TypeKindCPU {
					unit = "core"
				}
				return nil, shareMisfit(dimension.name, dimension.requested, shareQuantityString(dimension.key, share), g.AcceleratorCount, unit, cluster), nil
			}
		}
	}
	return &out, "", nil
}

// shareMisfit is the not_admitted_reason for a request above the share that count units (an
// accelerator, or a core for a CPU job) get of a node.
func shareMisfit(dimension, requested, share string, count int, unit, cluster string) string {
	return fmt.Sprintf("%s: %s %s is above the %s that %d %s(s) get on cluster %s (each node's %s split evenly across its installed %ss); request at most %s",
		domain.NotAdmittedExceedsAcceleratorShare, dimension, requested, share, count, unit, cluster, dimension, unit, share)
}

// shareQuantityString renders a share in canonical units as a JobSpec quantity no larger than it:
// memory and storage in Gi to one decimal, or Mi below 1Gi, rounded down so requesting it fits.
// Below 1Mi it is exact.
func shareQuantityString(key string, value int64) string {
	switch {
	case key == domain.NodeResourceCPUMillicores || value < 1<<20:
		return placement.QuantityString(key, float64(value))
	case value >= 1<<30:
		return strconv.FormatFloat(math.Floor(float64(value)*10/(1<<30))/10, 'f', -1, 64) + "Gi"
	}
	return strconv.FormatInt(value>>20, 10) + "Mi"
}

// joinMisfits is one not_admitted_reason for misfits, one per cluster tried, under the code of
// the first.
func joinMisfits(misfits []string) string {
	code, _ := domain.SplitQueueReason(misfits[0])
	sentences := make([]string, len(misfits))
	for i, m := range misfits {
		_, sentences[i] = domain.SplitQueueReason(m)
	}
	return code + ": " + strings.Join(sentences, "; ")
}
