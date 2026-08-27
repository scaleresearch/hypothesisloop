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
	resolveCache *resolutionCache,
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
		accelCeiling := nodeAvail[cluster]
		if len(nodeResourcesTotal[cluster]) > 0 {
			// A live node's FREE count is the wrong ceiling for this check (see fitsLargestNode's
			// doc comment) — reconstruct each node's INSTALLED count (free plus what the scheduler
			// itself has already claimed there) so a fully-saturated node still reports its real
			// capacity instead of the 0 that would wrongly reject the exact scenario this feature
			// exists for.
			installed, ierr := resolveCache.installedAcceleratorsByNode(ctx, cluster, exp.AcceleratorType, nodeAvail[cluster])
			if ierr != nil {
				return nil, ierr
			}
			accelCeiling = map[string]map[string]int64{}
			flavor := string(exp.AcceleratorType)
			for node, count := range installed {
				accelCeiling[node] = map[string]int64{flavor: count}
			}
		}
		if !fitsLargestNode(exp, accelCeiling, nodeResourcesTotal[cluster], nodeLabels[cluster]) {
			continue
		}
		cap, err := l.clusterSpeculativeCap(ctx, clusterID)
		if err != nil {
			return nil, err
		}
		if cap == nil && len(nodeResourcesTotal[cluster]) == 0 {
			// Zero live nodes means fitsLargestNode above accepted this cluster blind, with no
			// data at all backing the guess. Absent an operator-set cap, bound the exposure to one
			// job's own footprint rather than leaving it unbounded — once a real attempt lands
			// there (success or a tracked failover), the operator has empirical grounds to raise
			// max_speculative_accelerators if they want more concurrent bets on this cluster.
			oneJob := exp.AcceleratorCount
			cap = &oneJob
		}
		if cap != nil && speculativeFootprintByCluster[cluster]+exp.AcceleratorCount > *cap {
			continue
		}
		candidates = append(candidates, candidate{cluster: cluster, clusterID: clusterID})
	}
	// Fewest currently-pending speculative jobs first: spreads bets across eligible clusters
	// instead of piling every guaranteed job with nothing to fit onto the same one, using data
	// already computed this tick — no new state, no live-node dependency for the tiebreak either.
	sort.Slice(candidates, func(i, j int) bool {
		fi, fj := speculativeFootprintByCluster[candidates[i].cluster], speculativeFootprintByCluster[candidates[j].cluster]
		if fi != fj {
			return fi < fj
		}
		return candidates[i].clusterID < candidates[j].clusterID
	})
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
	v, ok := avail[key]
	if !ok {
		// A flavor this footprint never mentions is not "waiting for scale-up" — that is the
		// zero-value default doing the talking, not an observed desired-free of zero.
		return false
	}
	// <= 0, not < 0: a job needs at least one accelerator, so desired-free landing at exactly
	// zero (a speculative claim that exactly exhausted this flavor, not overdrew it) already
	// means "nothing here for the next guaranteed job either" -- codex review caught the
	// strictly-negative version as a boundary a speculative submit can legitimately land on
	// without ever going negative, which would have let preemption through right when the
	// skip-preemption rule exists to stop it.
	return v <= 0
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

// fitsLargestNode proves every rank of exp could plausibly land on SOME node this cluster's
// autoscaler could add, so a speculative submit is never made against a request no addable node
// would ever satisfy — that would Pend forever instead of triggering a useful scale-up. Judged per
// candidate node — a rank must fit one real node in every dimension at once, not a synthetic
// shape assembled from one node's CPU and another's accelerators — because an autoscaler adds
// nodes matching its existing node-group templates, and no template looks like a maximum-of-every-
// dimension composite unless some single node actually has that shape. Codex review caught the
// earlier per-dimension-max version as exactly this: it could pass a job that fits neither a
// CPU-heavy node nor a GPU-heavy node, onto a cluster with only those two, and the job would Pend
// forever since no addable node matches the synthesized shape.
//
// A cluster reporting zero live nodes (most commonly one already scaled to zero) has no shape data
// at all to check against — rather than excluding it forever (the old behaviour, and the reason
// the whole feature never fired for the scaled-to-zero and fully-saturated cases it exists for),
// it is accepted as a candidate blind. A bad guess here is bounded and self-healing: the very next
// tick's tried_clusters/infra_requeue_count backoff takes over the moment a speculative attempt
// there fails, and speculativeCandidates additionally defaults the per-cluster cap to one job's
// footprint for exactly this no-data case so a single bad guess can't pile up before it is proven
// wrong (see clusterSpeculativeCap's caller).
//
// For a cluster that DOES report live nodes, accelByNode must already carry each node's INSTALLED
// count (free plus whatever the scheduler itself has already claimed there), not raw free capacity
// — the caller (speculativeCandidates) reconstructs this via resolutionCache.
// installedAcceleratorsByNode before calling in. Gating on raw FREE count was the actual bug: a
// fully-saturated node (every device already in use, which is exactly the state that should
// trigger a scale-up bet) always reads free=0, so the old check rejected the one scenario the
// feature exists for.
func fitsLargestNode(exp *domain.Experiment, accelByNode, resourcesByNode map[string]map[string]int64, labelsByNode map[string]map[string]string) bool {
	if len(resourcesByNode) == 0 {
		return true
	}
	flavor := string(exp.AcceleratorType)
	sawLabelMatch := false
	for _, shape := range exp.NodeShapes() {
		fitsSomeNode := false
		for node, resources := range resourcesByNode {
			if !labelsMatch(labelsByNode[node], exp.Job.NodeSelector) {
				continue
			}
			sawLabelMatch = true
			if shape.AcceleratorCount <= accelByNode[node][flavor] && nodeHasRoom(resources, shape.Resources) {
				fitsSomeNode = true
				break
			}
		}
		if !fitsSomeNode {
			return false
		}
	}
	return sawLabelMatch
}
