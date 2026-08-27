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
	desiredFreeByCluster map[string]domain.Footprint,
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
		provenFit bool
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
		if negativeInDimension(desiredFreeByCluster[cluster], exp.AcceleratorType) {
			// This cluster's own desired-free already went negative in this job's accelerator
			// dimension: a SUBMITTED row is already outstanding here, i.e. a scale-up bet is
			// already in flight for exactly this shortage. Betting again here piles a second
			// speculative claim on top of the first instead of waiting for it to land — the
			// deadline in job_watcher_scan.go bounds the wait, this just avoids compounding it.
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
		// fitsLargestNode is a PREFERENCE signal, never an exclusion: a cluster can run several
		// heterogeneous node groups (different accelerator flavors, some scaled to zero) at once,
		// so "no currently-live node proves a fit" does not mean "this cluster's autoscaler could
		// never add a matching node" — it may just mean the matching node group is the one at zero
		// right now. Excluding on that would repeat the exact mistake this feature was fixed for,
		// one level up (per-node instead of per-cluster). Every autoscaler-enabled, non-backed-off
		// cluster stays a candidate; provenFit only affects ordering below.
		provenFit := fitsLargestNode(exp, accelCeiling, nodeResourcesTotal[cluster], nodeLabels[cluster])
		cap, err := l.clusterSpeculativeCap(ctx, clusterID)
		if err != nil {
			return nil, err
		}
		if cap == nil && !provenFit {
			// No live evidence backs this guess (either zero nodes, or live nodes that don't prove
			// this flavor/shape fits — a different node group might). Absent an operator-set cap,
			// bound the exposure to one job's own footprint rather than leaving it unbounded — once
			// a real attempt lands there (success or a tracked failover), the operator has empirical
			// grounds to raise max_speculative_accelerators if they want more concurrent bets here.
			oneJob := exp.AcceleratorCount
			cap = &oneJob
		}
		if cap != nil && speculativeFootprintByCluster[cluster]+exp.AcceleratorCount > *cap {
			continue
		}
		candidates = append(candidates, candidate{cluster: cluster, clusterID: clusterID, provenFit: provenFit})
	}
	// Clusters with live proof of fit go first (higher confidence bet), then fewest
	// currently-pending speculative jobs (spreads bets instead of piling onto one cluster), then a
	// stable clusterID tiebreak — all using data already computed this tick, no new state.
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].provenFit != candidates[j].provenFit {
			return candidates[i].provenFit
		}
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

// fitsLargestNode reports whether some CURRENTLY LIVE node in this cluster already proves exp
// could land there once the autoscaler adds a matching node — a confidence signal, not an
// eligibility gate (see speculativeCandidates, which never excludes on this). Judged per candidate
// node — a rank must fit one real node in every dimension at once, not a synthetic shape assembled
// from one node's CPU and another's accelerators — because an autoscaler adds nodes matching its
// existing node-group templates, and no template looks like a maximum-of-every-dimension composite
// unless some single node actually has that shape. Codex review caught the earlier
// per-dimension-max version as exactly this: it could pass a job that fits neither a CPU-heavy node
// nor a GPU-heavy node, onto a cluster with only those two, and the job would Pend forever since no
// addable node matches the synthesized shape.
//
// False does NOT mean "this cluster can never fit exp" — a cluster commonly runs several
// heterogeneous node groups at once (different accelerator flavors, some scaled to zero), and the
// node group that would actually fit may simply have no live nodes right now to prove it. Treating
// false as exclusion was the bug one level up from the original one: gating on a live node's
// capacity is exactly as wrong when done per-cluster as it was when done per-node-free-count. The
// caller only uses this to order candidates (proven-fit first) and to decide whether the default
// speculative cap applies — never to drop a cluster from consideration.
//
// A cluster reporting zero live nodes (most commonly one already scaled to zero) has no shape data
// at all to check against, so it reports false — no proof either way, exactly like a cluster whose
// live nodes are all the wrong flavor.
//
// For a cluster that DOES report live nodes, accelByNode must already carry each node's INSTALLED
// count (free plus whatever the scheduler itself has already claimed there), not raw free capacity
// — the caller (speculativeCandidates) reconstructs this via resolutionCache.
// installedAcceleratorsByNode before calling in. Gating on raw FREE count was the actual bug: a
// fully-saturated node (every device already in use, which is exactly the state that should
// trigger a scale-up bet) always reads free=0, so the old check wrongly read that as "no proof".
func fitsLargestNode(exp *domain.Experiment, accelByNode, resourcesByNode map[string]map[string]int64, labelsByNode map[string]map[string]string) bool {
	if len(resourcesByNode) == 0 {
		return false
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
