package scheduler

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/services/scheduler/placement"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/obsmetrics"
)

// scalesAbove reports whether a cluster of a higher priority than chosen could be asked to scale
// up for exp: connected, scaling, and taking the job.
func scalesAbove(chosen string, exp *domain.Experiment, autoscalerEnabled, connected map[string]bool, registered map[string]domain.Cluster) bool {
	for cluster := range autoscalerEnabled {
		if autoscalerEnabled[cluster] && connected[cluster] && hostsJob(registered, cluster, exp) && registered[cluster].Priority > registered[chosen].Priority {
			return true
		}
	}
	return false
}

// above keeps the clusters of a higher priority than chosen, in the order given.
func above(clusters []string, chosen string, registered map[string]domain.Cluster) []string {
	out := clusters[:0:0]
	for _, cluster := range clusters {
		if registered[cluster].Priority > registered[chosen].Priority {
			out = append(out, cluster)
		}
	}
	return out
}

// withoutFailed leaves out of a job's candidates the clusters the environment failed it on within
// ttl (domain.Experiment.TriedClusters), so its next placement is somewhere else. A cluster stays
// when leaving it out would leave nothing that could ever run the job: one that is the job's only
// compatible cluster is tried again at once, and the job's infrastructure allowance is what ends
// that. clusterIDs is each cluster's reported identity, which the tried list is kept under.
func withoutFailed(candidates map[string]domain.Footprint, clusterIDs map[string]string, exp *domain.Experiment, ttl time.Duration, now time.Time) map[string]domain.Footprint {
	if ttl <= 0 || len(exp.TriedClusters) == 0 {
		return candidates
	}
	kept := make(map[string]domain.Footprint, len(candidates))
	for cluster, footprint := range candidates {
		if id := clusterIDs[cluster]; id != "" && triedRecently(exp.TriedClusters, id, ttl, now) {
			continue
		}
		kept[cluster] = footprint
	}
	for _, footprint := range kept {
		if couldHost(footprint, exp) {
			return kept
		}
	}
	return candidates
}

// couldHost reports whether a cluster reporting capacity has the kind of hardware exp asks for,
// at the type it wants or one it accepts: present, not necessarily free.
func couldHost(capacity domain.Footprint, exp *domain.Experiment) bool {
	types := placement.CandidateTypes(exp)
	if len(types) == 0 {
		types = []domain.AcceleratorType{exp.AcceleratorType}
	}
	probe := *exp
	for _, t := range types {
		probe.AcceleratorType = t
		if placement.ReportsEveryDimension(capacity, probe.Footprint()) {
			return true
		}
	}
	return false
}

// tickClusters is what one tick read of the clusters, which every placement of both passes is
// decided against. The maps are the tick's own: an admission subtracts from them in place.
type tickClusters struct {
	start        time.Time
	registered   map[string]domain.Cluster
	multiNode    map[string]bool
	clusterIDs   map[string]string
	nodeLabels   map[string]map[string]map[string]string
	resolveCache *resolutionCache
	whenIdle     installed

	nodeAvail, nodeResources, nodeResourcesTotal, nodeAvailAtTickStart map[string]map[string]map[string]int64
}

// queuedPlacement is where a queued job is tried in one pass: the cluster, "" when none could be picked,
// and its footprint there. misfit is why the cluster picked does not take the job's own cpu,
// memory and storage, and betFlavor the flavor a bet on a cluster that scales up is made at.
type queuedPlacement struct {
	cluster   string
	fp        domain.Footprint
	betFlavor domain.AcceleratorType
	misfit    string
}

// placeQueued settles where exp is tried in one pass, for the guaranteed and the burst pass
// alike, against avail, the pass's view of free capacity. ok is false when the job is out of this
// tick: no cluster may run it, its quota no longer covers the type it ran on, or it could not be
// resolved, and the reason is written or the failure recorded.
//
// A job already assigned a cluster stays there and only its footprint is recomputed. Any other is
// given a (cluster, flavor) pair among the requested type and its acceptable ones where the whole
// footprint fits (placement.ClusterAndFootprint). The search says its choice by setting
// exp.AcceleratorType, and the resolved job is set on exp.
func (l *Loop) placeQueued(ctx context.Context, pass string, exp *domain.Experiment, avail map[string]domain.Footprint, quota *domain.AgentQuota, c tickClusters, tickErrs *[]error) (queuedPlacement, bool) {
	skip := func(reason string) (queuedPlacement, bool) {
		obsmetrics.AdmissionTickResultsTotal.WithLabelValues(pass, "skipped").Inc()
		if err := l.markNotAdmitted(ctx, exp, reason); err != nil {
			l.skipExperiment(tickErrs, exp, fmt.Errorf("mark not admitted: %w", err))
		}
		return queuedPlacement{}, false
	}
	placed := queuedPlacement{cluster: exp.ClusterName, betFlavor: exp.AcceleratorType}
	candidates, ineligible := eligibleClusters(avail, c.multiNode, c.registered, exp)
	if ineligible != "" {
		return skip(ineligible)
	}
	candidates = withoutFailed(candidates, c.clusterIDs, exp, l.triedClusterTTL, c.start)
	candidates = c.whenIdle.couldHold(candidates, exp)
	pinned := placed.cluster != ""
	if pinned {
		placed.fp = exp.Footprint()
	}
	// Check the job's cpu, memory and storage against the picked cluster's own fair share (or size
	// it to a slot), once cluster and accelerator flavor are settled: see loop_resolve.go. An
	// unpinned job whose own resources the cluster cannot fit is placed again among the rest, and
	// is told it misfits only when every candidate misfits: preemption cannot fix a job whose own
	// request is oversized for every node.
	var misfits []string
	queuedAt := exp.AcceleratorType
	for {
		if !pinned {
			exp.AcceleratorType, placed.betFlavor = queuedAt, queuedAt
			placed.cluster, placed.fp = placement.ClusterAndFootprint(candidates, c.registered, c.nodeAvail, c.nodeResources, c.nodeLabels, exp)
		}
		if exp.Job.AcceleratorType != "" && exp.AcceleratorType != exp.Job.AcceleratorType && !quotaCanCoverFlavor(quota, exp) {
			if !slices.Contains(placement.CandidateTypes(exp), exp.Job.AcceleratorType) {
				return skip(ranOnTypeUncoveredReason(exp))
			}
			// Reverting to the requested flavor re-places the job, layout included: picking on
			// scalar fit alone could point a spread job at a cluster whose nodes cannot hold it,
			// and charge it a failed placement there.
			exp.AcceleratorType = exp.Job.AcceleratorType
			placed.betFlavor = exp.AcceleratorType
			placed.fp = exp.Footprint()
			if cluster, ok := placement.AtFlavor(candidates, c.registered, c.nodeAvail, c.nodeResources, c.nodeLabels, exp); ok {
				placed.cluster = cluster
			} else {
				placed.cluster = placement.BestFit(candidates, c.registered, placed.fp)
			}
		}
		cluster := placed.cluster
		if cluster == "" {
			break
		}
		resolvedJob, misfit, err := l.resolveClusterLocalResources(ctx, c.resolveCache, exp, cluster, c.nodeResourcesTotal[cluster], c.nodeAvailAtTickStart[cluster], c.nodeResources[cluster], c.nodeLabels[cluster])
		if err != nil {
			l.skipExperiment(tickErrs, exp, fmt.Errorf("resolve cluster-local resources: %w", err))
			return queuedPlacement{}, false
		}
		if misfit == "" {
			exp.ResolvedJob = resolvedJob
			placed.fp = exp.Footprint()
			return placed, true
		}
		misfits = append(misfits, misfit)
		placed.cluster = ""
		if pinned {
			break
		}
		candidates = maps.Clone(candidates)
		delete(candidates, cluster)
	}
	if len(misfits) > 0 {
		placed.misfit = joinMisfits(misfits)
	}
	return placed, true
}

// holdRoom keeps what cluster has free from every job after exp in this tick, once exp is held
// (MarkHeldJobs): it has waited longer than heldJobNotice and than its own estimate. Smaller jobs
// behind it otherwise take each core as it frees and exp never starts. One job holds a cluster per
// tick, and only a cluster that holds it when idle, so the room left idle is one waiting job's.
func (l *Loop) holdRoom(exp *domain.Experiment, cluster string, fp domain.Footprint, c tickClusters, held map[string]bool, views ...map[string]domain.Footprint) {
	if l.heldJobNotice <= 0 || exp.QueuedAt == nil || held[cluster] || placement.HasSlots(c.nodeLabels[cluster]) {
		return
	}
	if waited := c.start.Sub(*exp.QueuedAt); waited < l.heldJobNotice || waited.Hours() < exp.EstimatedDurationHours {
		return
	}
	if total, ok := c.whenIdle.totals[cluster]; !ok || !placement.FitsWhenIdle(total, c.whenIdle.nodeTotals[cluster], exp) {
		return
	}
	held[cluster] = true
	for _, view := range views {
		placement.SubtractFootprint(view[cluster], fp)
	}
}

// installed is what the clusters have with nothing running on them. A job is held to it at submit
// and in the queue by the one function that reads it, neverFitsReason.
type installed struct {
	registered map[string]domain.Cluster
	// totals has a cluster only while it is connected and reports all it has.
	totals     map[string]domain.Footprint
	nodeTotals map[string]map[string]map[string]int64
	scalesUp   map[string]bool
	multiNode  map[string]bool
}

// neverFitsReason is the not_admitted_reason of a job that no wait admits as it is stated, and ""
// for one that may yet run. Such a job asks for more than every cluster that runs its image
// choice and reports what it has installed holds when idle (placement.FitsWhenIdle), on each accelerator
// type the job accepts. A cluster that scales up may yet hold it. A cluster that reports nothing
// is away or not whole: it is named and not judged, and with none judged the job waits.
//
// The cause is the job's image where a cluster that runs another holds the job when idle, and its
// size otherwise. Both say what each cluster judged has.
func (in installed) neverFitsReason(exp *domain.Experiment) string {
	runs, others := byImageChoice(in.registered, exp)
	if len(runs) == 0 {
		return noClusterRunsImageReason(others, exp)
	}
	spansNodes := exp.Job.Nodes() > 1
	var judged []string
	lacking := 0
	for _, c := range runs {
		total, reported := in.totals[c.Name]
		switch {
		case !reported:
			judged = append(judged, c.Name+" reports nothing now")
		case spansNodes && !in.multiNode[c.Name]:
		case in.scalesUp[c.Name] || placement.FitsWhenIdle(total, in.nodeTotals[c.Name], exp):
			return ""
		default:
			lacking++
			judged = append(judged, c.Name+" has "+describeOffer(total, in.nodeTotals[c.Name], exp))
		}
	}
	if lacking == 0 {
		return ""
	}
	var holding []string
	for _, c := range others {
		total, reported := in.totals[c.Name]
		if reported && (!spansNodes || in.multiNode[c.Name]) && placement.FitsWhenIdle(total, in.nodeTotals[c.Name], exp) {
			holding = append(holding, c.Name+" runs every job in "+c.JobImage)
		}
	}
	asks := describeFootprint(exp.Footprint())
	if spansNodes {
		asks += fmt.Sprintf(" over %d nodes", exp.Job.Nodes())
	}
	has := strings.Join(judged, "; ")
	if len(holding) == 0 {
		return fmt.Sprintf("%s: the job asks for %s, more than any connected cluster that runs it has when idle: %s. Ask for less (hl capacity lists what each cluster has)",
			domain.NotAdmittedExceedsClusterCapacity, asks, has)
	}
	return fmt.Sprintf("%s: the job names image %s, and no connected cluster that runs it has what the job asks for, %s, when idle: %s. A cluster with one job image has it (%s): %s",
		domain.NotAdmittedNoClusterForImage, exp.Job.Image, asks, has, strings.Join(holding, "; "), nameThatImage)
}

// couldHold narrows candidates to the clusters that hold exp when idle, so a job that fits nowhere
// now waits on, and is held room on, a cluster it can run on. A cluster that reports no totals is
// kept, and all of candidates when none is left.
func (in installed) couldHold(candidates map[string]domain.Footprint, exp *domain.Experiment) map[string]domain.Footprint {
	kept := make(map[string]domain.Footprint, len(candidates))
	for cluster, free := range candidates {
		if total, ok := in.totals[cluster]; !ok || placement.FitsWhenIdle(total, in.nodeTotals[cluster], exp) {
			kept[cluster] = free
		}
	}
	if len(kept) == 0 {
		return candidates
	}
	return kept
}

// eligibleClusters narrows the clusters a job may be placed on to the ones that may and can run
// it: registered clusters of the job's own org and shared ones that the org has not turned off,
// and of those, for a job that spans nodes, the ones that report multi-node execution. This is
// the one place the platform decides which org's jobs a cluster takes.
//
// The returned map shares its Footprint values with avail rather than copying them, so the
// in-place reservations admission makes against the returned view are the same numbers every
// later job in this tick reads. A copy here would let a job admitted through the narrowed view
// be double-booked by one placed through the full one.
//
// ineligible is the not_admitted_reason when a filter is what emptied the result, and it is the
// only thing that makes an empty result diagnosable. Whether the job's org has a cluster at all
// is read from what is registered, never from what is connected: an org whose own cluster is
// away has a cluster, and what other orgs have connected is not its business to learn. With no
// cluster of its own connected a single-node job gets an empty set and no reason: that is a
// heartbeat gap, and reporting it as a missing cluster or a missing capability would tell the
// submitter to fix something that is not broken.
func eligibleClusters(avail map[string]domain.Footprint, multiNodeCapable map[string]bool, registered map[string]domain.Cluster, exp *domain.Experiment) (clusters map[string]domain.Footprint, ineligible string) {
	hosted := make(map[string]domain.Footprint, len(avail))
	for cluster, footprint := range avail {
		if hostsJob(registered, cluster, exp) {
			hosted[cluster] = footprint
		}
	}
	// A job never reaches the queue holding a cluster, so this refuses only a pin written
	// around admission.
	pinnedElsewhere := exp.ClusterName != "" && !hostsJob(registered, exp.ClusterName, exp)
	if pinnedElsewhere || !orgHasCluster(registered, exp.OrgID, domain.Cluster.OpenTo) {
		return map[string]domain.Footprint{}, noClusterForOrgReason()
	}
	if !orgHasCluster(registered, exp.OrgID, domain.Cluster.Hosts) {
		return map[string]domain.Footprint{}, clustersDisabledReason()
	}
	if runs, others := byImageChoice(registered, exp); len(runs) == 0 {
		return map[string]domain.Footprint{}, noClusterRunsImageReason(others, exp)
	}
	if exp.Job.Nodes() <= 1 {
		return hosted, ""
	}
	out := make(map[string]domain.Footprint, len(hosted))
	for cluster, footprint := range hosted {
		if multiNodeCapable[cluster] {
			out[cluster] = footprint
		}
	}
	if len(out) == 0 {
		return out, noMultiNodeClusterReason(exp.Job.Nodes())
	}
	return out, ""
}

// orgHasCluster reports whether any registered cluster, connected or not, is the org's in the
// given sense: open to it (domain.Cluster.OpenTo), or taking its jobs (domain.Cluster.Hosts).
func orgHasCluster(registered map[string]domain.Cluster, orgID string, is func(domain.Cluster, string) bool) bool {
	for _, cluster := range registered {
		if is(cluster, orgID) {
			return true
		}
	}
	return false
}

// hostsJob reports whether cluster is registered, takes jobs of exp's org and runs the image exp
// names (domain.Cluster.HostsJob).
func hostsJob(registered map[string]domain.Cluster, cluster string, exp *domain.Experiment) bool {
	c, ok := registered[cluster]
	return ok && c.HostsJob(exp.OrgID, exp.Job.Image)
}

// byImageChoice splits the clusters that take jobs of exp's org into those that run the image
// exp names, or run it naming none (domain.Cluster.Runs), and the others, each sorted by name.
func byImageChoice(registered map[string]domain.Cluster, exp *domain.Experiment) (runs, others []domain.Cluster) {
	for _, c := range registered {
		switch {
		case !c.Hosts(exp.OrgID):
		case c.Runs(exp.Job.Image):
			runs = append(runs, c)
		default:
			others = append(others, c)
		}
	}
	byName := func(a, b domain.Cluster) int { return strings.Compare(a.Name, b.Name) }
	slices.SortFunc(runs, byName)
	slices.SortFunc(others, byName)
	return runs, others
}

// quotaCanCoverFlavor is an early, non-authoritative filter for a more expensive acceptable
// flavor; ReserveAdmittedFlavor's transaction remains the final concurrency-safe check.
func quotaCanCoverFlavor(quota *domain.AgentQuota, exp *domain.Experiment) bool {
	if quota == nil || exp.AcceleratorCount <= 0 {
		return false
	}
	newCost, err := exp.EstimateHeld()
	if err != nil {
		return false
	}
	used, limit := quota.UsedBurstAccH, quota.BurstAcceleratorHours
	if exp.CapacityTier == domain.CapacityGuaranteed {
		used, limit = quota.UsedGuaranteedAccH, quota.GuaranteedAcceleratorHours
	}
	return used-exp.EstimatedCostAccH+newCost <= limit && quota.Used()-exp.EstimatedCostAccH+newCost <= quota.Share()
}
