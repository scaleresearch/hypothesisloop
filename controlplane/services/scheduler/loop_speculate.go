package scheduler

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// speculativeCandidates implements autoscaler.md's step 2 candidate search: which
// autoscaler-enabled clusters could plausibly host exp once a native autoscaler adds a node,
// even though nothing free exists there right now. Called only from the guaranteed pass, only
// after live no-fit is established — burst jobs never speculate (autoscaler.md: "a burst job
// should not boot a node it can be preempted off").
//
// Ordering is stable by cluster_id (autoscaler.md: "no ranking machinery") — the caller picks
// candidates[0].
func (l *Loop) speculativeCandidates(
	ctx context.Context,
	exp *domain.Experiment,
	autoscalerEnabled map[string]bool,
	connected map[string]bool,
	clusterIDs map[string]string,
	multiNodeCapable map[string]bool,
	nodeAvail map[string]map[string]map[string]int64,
	nodeResourcesTotal map[string]map[string]map[string]int64,
	nodeLabels map[string]map[string]map[string]string,
	speculativeFootprintByCluster map[string]int,
) ([]string, error) {
	if l.triedClusterTTL <= 0 {
		// WithSpeculation was never called: this deployment has not opted in, so admission keeps
		// today's live-fit-only behaviour exactly (see loop_types.go's triedClusterTTL doc).
		return nil, nil
	}
	gang := requiresDistinctHosts(exp) || len(exp.Job.Groups) > 0 || exp.Job.Nodes() > 1
	now := time.Now()

	// Cross-job half of the tried-cluster backoff (autoscaler.md line 109): a cluster any job
	// failed over from within the TTL is excluded here too, not just this job's own tried-list —
	// otherwise every queued job lines up behind the same dead node group in turn instead of
	// skipping it together.
	recentlyTried, err := l.store.RecentlyTriedClusters(ctx, l.triedClusterTTL)
	if err != nil {
		return nil, err
	}

	type candidate struct {
		cluster   string
		clusterID string
	}
	var candidates []candidate
	for cluster, clusterID := range clusterIDs {
		if clusterID == "" || !autoscalerEnabled[cluster] || !connected[cluster] {
			continue
		}
		if gang && !multiNodeCapable[cluster] {
			continue
		}
		if triedRecently(exp.TriedClusters, clusterID, l.triedClusterTTL, now) {
			continue
		}
		if recentlyTried[clusterID] {
			continue
		}
		if !fitsLargestNode(exp, nodeAvail[cluster], nodeResourcesTotal[cluster], nodeLabels[cluster]) {
			continue
		}
		if cap, err := l.clusterSpeculativeCap(ctx, clusterID); err != nil {
			return nil, err
		} else if cap != nil && speculativeFootprintByCluster[cluster]+exp.AcceleratorCount > *cap {
			continue
		}
		candidates = append(candidates, candidate{cluster: cluster, clusterID: clusterID})
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].clusterID < candidates[j].clusterID })
	out := make([]string, len(candidates))
	for i, c := range candidates {
		out[i] = c.cluster
	}
	return out, nil
}

// clusterSpeculativeCap reads max_speculative_accelerators from cluster_settings, nil meaning no
// per-cluster cap (only the platform experiment's max_concurrent_accelerators bounds it — see
// autoscaler.md's concurrency-cap section, enforced in step 6, not here).
func (l *Loop) clusterSpeculativeCap(ctx context.Context, clusterID string) (*int, error) {
	cs, err := l.store.GetClusterSettings(ctx, clusterID)
	if err != nil {
		return nil, err
	}
	if cs == nil {
		return nil, nil
	}
	return cs.MaxSpeculativeAccelerators, nil
}

// triedRecently reports whether exp already failed a speculative attempt on clusterID within
// ttl. The list only ever grows (see domain.Experiment.TriedClusters); entries simply age out.
func triedRecently(tried []domain.TriedCluster, clusterID string, ttl time.Duration, now time.Time) bool {
	for _, t := range tried {
		if t.ClusterID == clusterID && now.Sub(t.At) < ttl {
			return true
		}
	}
	return false
}

// negativeInDimension reports whether avail already carries a negative desired-free value for
// flavor — i.e. someone already has a SUBMITTED row outstanding against this cluster's capacity
// in this dimension. domain.Footprint.Sub never clamps at zero, so GetFlavorCapacity's own
// output already answers this with no second query.
func negativeInDimension(avail domain.Footprint, flavor domain.AcceleratorType) bool {
	if avail == nil {
		return false
	}
	key := domain.ResourceKey{Kind: domain.ResourceKindAccelerator, Flavor: strings.ToLower(string(flavor))}
	return avail[key] < 0
}

// allSpeculativeCandidatesTried reports whether at least one autoscaler-enabled, connected
// cluster exists for exp, and every one of them is within exp's own tried-list TTL window — i.e.
// speculation has nowhere left to go right now, not because no autoscaler cluster exists at all.
// Used only to pick the not-admitted reason (no_scalable_capacity vs the ordinary
// capacity_unavailable/outranked reasons); it does not gate any admission decision.
func allSpeculativeCandidatesTried(exp *domain.Experiment, autoscalerEnabled, connected map[string]bool, clusterIDs map[string]string, ttl time.Duration) bool {
	if ttl <= 0 {
		return false
	}
	now := time.Now()
	sawCandidate := false
	for cluster, clusterID := range clusterIDs {
		if clusterID == "" || !autoscalerEnabled[cluster] || !connected[cluster] {
			continue
		}
		sawCandidate = true
		if !triedRecently(exp.TriedClusters, clusterID, ttl, now) {
			return false
		}
	}
	return sawCandidate
}

// fitsLargestNode proves every rank of exp could fit SOME node in this cluster's own pool, so a
// speculative submit is never made against a request no node the autoscaler could add would ever
// satisfy — that would Pend forever instead of triggering a useful scale-up. Judged against the
// largest node currently known to the cluster in each dimension independently (an autoscaler adds
// nodes matching its existing node-group templates, so an existing node is the best available
// proxy for "what a new node looks like"). A cluster reporting zero nodes never speculates.
//
// Per-node accelerator ceilings use nodeAvail (currently FREE devices) rather than an installed
// total, because the control plane has no per-node installed-accelerator metric (see
// autoscaler.md's fact table: only accelerator_available_by_node is reported). This
// under-estimates a node whose accelerators are entirely in use, which can only make this check
// too strict, never too lenient — the failure direction that matters here is never speculating
// onto a node shape that could not actually exist.
func fitsLargestNode(exp *domain.Experiment, accelByNode, resourcesByNode map[string]map[string]int64, labelsByNode map[string]map[string]string) bool {
	if len(resourcesByNode) == 0 {
		return false
	}
	flavor := string(exp.AcceleratorType)
	maxAccel := int64(0)
	maxResources := map[string]int64{}
	sawLabelMatch := false
	for node, resources := range resourcesByNode {
		if !labelsMatch(labelsByNode[node], exp.Job.NodeSelector) {
			continue
		}
		sawLabelMatch = true
		for key, amount := range resources {
			if amount > maxResources[key] {
				maxResources[key] = amount
			}
		}
		if a := accelByNode[node][flavor]; a > maxAccel {
			maxAccel = a
		}
	}
	if !sawLabelMatch {
		return false
	}
	for _, shape := range exp.NodeShapes() {
		if shape.AcceleratorCount > maxAccel {
			return false
		}
		if !nodeHasRoom(maxResources, shape.Resources) {
			return false
		}
	}
	return true
}
