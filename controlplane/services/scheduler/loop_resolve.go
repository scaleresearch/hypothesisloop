package scheduler

import (
	"context"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// resolutionCache amortizes the running-experiment node-attribution lookups tick-time "max"
// resolution needs (see resolveClusterLocalResources) across every queued job considered in one
// tick: the running set and each running job's node are the same answer for every queued job
// asking about a given accelerator flavor, so this is built lazily and reused rather than
// re-queried once per queued job.
type resolutionCache struct {
	l             *Loop
	claimed       []*domain.Experiment // RUNNING, SUBMITTED, and ADMITTED — see loadClaimed
	claimedLoaded bool
	nodeOf        map[string]string // experiment ID -> node, filled lazily
}

func newResolutionCache(l *Loop) *resolutionCache {
	return &resolutionCache{l: l, nodeOf: map[string]string{}}
}

// loadClaimed returns every experiment that has already claimed accelerator capacity toward the
// nodes it landed on — RUNNING, SUBMITTED, and ADMITTED, not just RUNNING. A RUNNING-only view
// undercounts: SUBMITTED/ADMITTED jobs have already reduced nodeAvail's free count but have not
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
// RUNNING, SUBMITTED, or ADMITTED experiment (see loadClaimed — SUBMITTED/ADMITTED jobs have
// already reduced nodeAvail's free count and must be counted here too, or "installed" undercounts
// and every FairShare computed against it is wrong), attributed the same way loop_disbalance.go's
// disbalance evictor attributes running work to a node — one metrics-store node-attribution query
// per claimed experiment of this flavor, cached across the whole tick (see resolutionCache's doc
// comment), not per queued job.
//
// Node attribution (metricsdb.LatestExperimentNode) is keyed by observed placement, so a
// SUBMITTED/ADMITTED job the backend has not yet scheduled anywhere may not resolve to a node yet
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
func (c *resolutionCache) installedAcceleratorsByNode(ctx context.Context, cluster string, flavor domain.AcceleratorType, nodeAvail map[string]map[string]int64) (map[string]int64, error) {
	key := strings.ToLower(string(flavor))
	installed := make(map[string]int64, len(nodeAvail))
	for node, byFlavor := range nodeAvail {
		installed[node] = foldLookup(byFlavor, key)
	}
	claimed, err := c.loadClaimed(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve cluster-local resources: list capacity-claimed experiments: %w", err)
	}
	for _, r := range claimed {
		if r.ClusterName != cluster || r.AcceleratorCount <= 0 {
			continue
		}
		if !strings.EqualFold(string(r.AcceleratorType), string(flavor)) {
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

// resolveClusterLocalResources resolves any "max" CPU/memory/storage sentinel in exp.Job (or its
// groups) into a literal quantity, and validates any EXPLICIT number, against exp's cluster-local
// fair share within `cluster` — the MINIMUM domain.FairShare across every node in `cluster` that
// is topology-eligible for the group in question (matches exp's accelerator flavor, exp.Job's
// node_selector, and currently reports live capacity). Runs once cluster and accelerator flavor
// are already decided for this attempt (see loop_tick.go's placement search that calls this) —
// never before, since "max" resolved against the wrong cluster's fair share is meaningless the
// moment placement moves to a different one.
//
// Resolution is PER GROUP: a grouped job's groups have their own per-node accelerator count
// (Replicas), and each group's fair share is computed against that group's own count, never the
// job's TotalAccelerators. A group requesting zero accelerators has no per-accelerator share to
// compute and is left exactly as submitted (see JobSpec.ValidateGroups: such a group's CPU cannot
// be "max" — see ValidateExperiment's structural check).
//
// fits=false means this job's resources, at this cluster, this tick, do not work: either an
// explicit number exceeds every eligible node's fair share, or no node in `cluster` is currently
// eligible at all to resolve or validate against. Both cases are "doesn't fit this cluster/tick",
// exactly like an ordinary scalar or topology capacity miss — the caller folds it into the same
// not-admitted path (never a hard rejection; a later tick, or a different cluster, may still work)
// rather than treating it as malformed. Reserving hard rejection for "this accelerator type exists
// nowhere in the whole fleet" happens once, cheaply, at submission (see ValidateExperiment) — not
// here, where the type is already known to exist somewhere.
func (l *Loop) resolveClusterLocalResources(ctx context.Context, cache *resolutionCache, exp *domain.Experiment, cluster string, nodeResourcesTotal, nodeAvail map[string]map[string]int64, nodeLabels map[string]map[string]string) (resolved *domain.JobSpec, fits bool, err error) {
	out := exp.Job
	if len(exp.Job.Groups) > 0 {
		out.Groups = append([]domain.JobGroup(nil), exp.Job.Groups...)
	}

	groups := exp.Job.NodeGroups()
	needsAccelerator := false
	for _, g := range groups {
		if g.AcceleratorCount > 0 {
			needsAccelerator = true
			break
		}
	}
	if !needsAccelerator {
		// No accelerator dimension anywhere in this job to compute a per-accelerator share
		// against — CPU-only jobs never carry "max" (see ValidateExperiment) and are never
		// subject to the proportionate-share check, exactly as before this feature existed.
		return &out, true, nil
	}

	installed, ierr := cache.installedAcceleratorsByNode(ctx, cluster, exp.AcceleratorType, nodeAvail)
	if ierr != nil {
		return nil, false, ierr
	}

	for gi, g := range groups {
		if g.AcceleratorCount <= 0 {
			continue
		}
		if g.CPU != domain.MaxResourceSentinel && g.Memory != domain.MaxResourceSentinel && g.Storage != domain.MaxResourceSentinel {
			// Nothing to resolve: every dimension is already an explicit number, so this group
			// needs no cluster-local fair share at all. Requiring one anyway would wrongly tie an
			// already-fully-specified job to "does some live node here match this flavor" — the
			// same live-node dependency the speculative-scheduling fix just removed one level up
			// (speculativeCandidates), for a cluster that may simply have the matching node group
			// at zero nodes right now (see loop_speculate.go's fitsLargestNode doc comment).
			continue
		}
		var cpuShare, memShare, storageShare int64
		haveBound := false
		for node, total := range nodeResourcesTotal {
			if !labelsMatch(nodeLabels[node], exp.Job.NodeSelector) {
				continue
			}
			nodeInstalled := installed[node]
			if nodeInstalled <= 0 {
				continue // this node does not currently report the job's accelerator flavor at all
			}
			c, err := domain.FairShare(total[domain.NodeResourceCPUMillicores], int64(g.AcceleratorCount), nodeInstalled)
			if err != nil {
				return nil, false, fmt.Errorf("resolve cluster-local resources: cpu fair share: %w", err)
			}
			m, err := domain.FairShare(total[domain.NodeResourceMemoryBytes], int64(g.AcceleratorCount), nodeInstalled)
			if err != nil {
				return nil, false, fmt.Errorf("resolve cluster-local resources: memory fair share: %w", err)
			}
			s, err := domain.FairShare(total[domain.NodeResourceStorageBytes], int64(g.AcceleratorCount), nodeInstalled)
			if err != nil {
				return nil, false, fmt.Errorf("resolve cluster-local resources: storage fair share: %w", err)
			}
			if !haveBound || c < cpuShare {
				cpuShare = c
			}
			if !haveBound || m < memShare {
				memShare = m
			}
			if !haveBound || s < storageShare {
				storageShare = s
			}
			haveBound = true
		}
		if !haveBound {
			if g.CPU != domain.MaxResourceSentinel && g.Memory != domain.MaxResourceSentinel && g.Storage != domain.MaxResourceSentinel {
				// No node in `cluster` is currently eligible to validate this group's EXPLICIT
				// numbers against — this is exactly the speculative-blind case (a candidate with no
				// live proof of fit, or no live nodes at all: see fitsLargestNode/speculativeCandidates).
				// There's nothing to over-fit here since nothing is being resolved, only validated;
				// refusing to even attempt the submission would repeat the bug this fix addresses one
				// layer deeper. Leave the group exactly as submitted and let the real cluster-local
				// fair share (once a node actually appears there) catch an oversized explicit request
				// on the next tick via the ordinary scalar/topology capacity check.
				continue
			}
			// An unresolved "max" genuinely cannot be guessed without any live node to measure a
			// share against — this is the one case still folded into "doesn't fit this cluster/tick".
			return nil, false, nil
		}
		cpu, err := resolveOrValidateDimension(g.CPU, domain.NodeResourceCPUMillicores, cpuShare)
		if err != nil {
			return nil, false, nil
		}
		mem, err := resolveOrValidateDimension(g.Memory, domain.NodeResourceMemoryBytes, memShare)
		if err != nil {
			return nil, false, nil
		}
		storage, err := resolveOrValidateDimension(g.Storage, domain.NodeResourceStorageBytes, storageShare)
		if err != nil {
			return nil, false, nil
		}
		if len(out.Groups) > 0 {
			out.Groups[gi].CPU, out.Groups[gi].Memory, out.Groups[gi].Storage = cpu, mem, storage
		} else {
			out.CPU, out.Memory, out.Storage = cpu, mem, storage
		}
	}
	return &out, true, nil
}

// resolveOrValidateDimension returns qty unchanged unless it is domain.MaxResourceSentinel (in
// which case it resolves to bound), and errors if an explicit qty exceeds bound.
func resolveOrValidateDimension(qty, canonicalKey string, bound int64) (string, error) {
	if qty == domain.MaxResourceSentinel {
		return canonicalQuantityString(canonicalKey, float64(bound)), nil
	}
	value, err := canonicalQuantityValue(canonicalKey, qty)
	if err != nil {
		// A malformed quantity is ValidateExperiment's job to reject; do not duplicate it here.
		return qty, nil
	}
	if value > float64(bound) {
		return "", fmt.Errorf("%s exceeds cluster-local fair share of %s", qty, canonicalQuantityString(canonicalKey, float64(bound)))
	}
	return qty, nil
}

// jobNeedsMaxResolution reports whether any CPU/memory/storage field of job (top-level or any
// group's own) still carries the literal domain.MaxResourceSentinel — used by the manual
// operator /admit endpoint (handler.go), which bypasses the ordinary tick's cluster-local
// resolution entirely, to refuse rather than silently admit against a footprint that reads every
// unresolved "max" field as zero.
func jobNeedsMaxResolution(job domain.JobSpec) bool {
	for _, g := range job.NodeGroups() {
		if g.CPU == domain.MaxResourceSentinel || g.Memory == domain.MaxResourceSentinel || g.Storage == domain.MaxResourceSentinel {
			return true
		}
	}
	return false
}

// canonicalQuantityValue parses a JobSpec-style quantity string into the canonical unit
// domain.NodeResource* uses for key (millicores for CPU, bytes for memory/storage).
func canonicalQuantityValue(key, qty string) (float64, error) {
	q, err := resource.ParseQuantity(qty)
	if err != nil {
		return 0, err
	}
	if key == domain.NodeResourceCPUMillicores {
		return float64(q.MilliValue()), nil
	}
	return float64(q.Value()), nil
}

// canonicalQuantityString renders a canonical-unit value (millicores for CPU, bytes for
// memory/storage) back into a JobSpec-style quantity string.
func canonicalQuantityString(key string, value float64) string {
	if key == domain.NodeResourceCPUMillicores {
		return resource.NewMilliQuantity(int64(value), resource.DecimalSI).String()
	}
	return resource.NewQuantity(int64(value), resource.BinarySI).String()
}
