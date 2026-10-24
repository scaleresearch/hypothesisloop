package scheduler

import (
	"fmt"
	"strings"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/scheduler/placement"
)

// Every not_admitted_reason is stored as "code: sentence" (domain.SplitQueueReason). The sentence
// is what job.held and not_admitted_message carry, so a reason written without one tells the
// job's owner nothing. The reasons below are the ones whose sentence needs little of the job; the
// rest are worded where they are decided.

func jobTooLongReason(estimatedHours, limitHours float64) string {
	return fmt.Sprintf("%s: its estimated_duration_hours %g is over the %g h one job may take in the platform experiment's current stage: cancel it and submit shorter runs, or it waits for a stage that allows its length",
		domain.NotAdmittedJobTooLong, estimatedHours, limitHours)
}

func stageCutReason() string {
	return domain.NotAdmittedStageCut + ": its agent was cut at a stage of the platform experiment, and a cut agent's queued jobs are not started"
}

func waitingForScaleUpReason(cluster string) string {
	return fmt.Sprintf("%s: cluster %s was asked for more machines of this type and they have not joined yet: the job is admitted when they do", domain.NotAdmittedWaitingForScaleUp, cluster)
}

func noScalableCapacityReason() string {
	return domain.NotAdmittedNoScalableCapacity + ": no cluster has room for it now, and every cluster that scales up was asked for it lately and brought no machine: it is tried again as room frees or those tries expire"
}

func noClusterForOrgReason() string {
	return domain.NotAdmittedNoClusterForOrg + ": no cluster takes this org's jobs, none is its own and none is shared: an operator registers or shares one, and no change to the job helps"
}

func clustersDisabledReason() string {
	return domain.NotAdmittedClustersDisabled + ": the org turned off every cluster it could use: turning one on admits the job, and no change to the job helps"
}

func noMultiNodeClusterReason(nodes int) string {
	return fmt.Sprintf("%s: the job spans %d nodes, and no cluster that takes this org's jobs runs a job on more than one", domain.NotAdmittedNoMultiNodeCluster, nodes)
}

func concurrencyCapReason() string {
	return domain.NotAdmittedConcurrencyCap + ": the platform experiment has as many accelerators in flight as its max_concurrent_accelerators allows: the job is admitted as its running jobs end"
}

func capacityTakenReason() string {
	return domain.NotAdmittedCapacityUnavailable + ": another job took the room while this one was being admitted: it is looked at again within seconds"
}

func workloadCreationReason() string {
	return domain.NotAdmittedWorkloadCreation + ": the platform could not hand the job to its cluster: it is tried again within seconds"
}

// notAdmittedReasonFor names why a queued job was skipped, as "code: message". fitAtTickStart is
// the whole discriminator: outranked means the job *would* have been admitted against the
// capacity this tick started with and lost it to jobs earlier in sort order, so waiting is the
// right response. A request oversized for the hardware is never told it was outranked.
//
// scalarFits is the case waiting on the cluster total cannot explain: the total has room and no
// arrangement of the job's nodes over the cluster's nodes holds it. cluster is the one the job
// was judged on, "" when none could be picked, and held the jobs that hold there what it is
// short of.
//
// freedInDesiredState is the case the cluster's own figures cannot explain yet: with every job
// the platform has placed there counted, the cluster has room for this one, and its last capacity
// report still shows less. Its totals are net of everything that is no platform job, so what is
// missing is a job that ended and that the runtime has not reported gone. That holds for cores,
// memory and storage. A device may be held by something the platform never placed, so a job
// short of one is not told a report will free it.
func notAdmittedReasonFor(fitAtTickStart, scalarFits, freedInDesiredState bool, shortage domain.Footprint, cluster string, held holders) string {
	if fitAtTickStart {
		return domain.NotAdmittedOutranked + ": it fit when this pass began, and jobs ahead of it in the queue took the room"
	}
	short := describeFootprint(shortage)
	switch {
	case cluster == "" && short == "":
		return domain.NotAdmittedCapacityUnavailable + ": no cluster that runs it has room for it now"
	case cluster == "":
		return domain.NotAdmittedCapacityUnavailable + ": no cluster that runs it has room for it now: it is short of " + short
	case scalarFits && short == "":
		return fmt.Sprintf("%s: cluster %s has room for it in total and no node of it holds the job", domain.NotAdmittedCapacityUnavailable, cluster)
	case scalarFits:
		return fmt.Sprintf("%s: cluster %s has room for it in total and no node of it holds the job: the node nearest to holding it is short of %s", domain.NotAdmittedCapacityUnavailable, cluster, short)
	case short == "":
		return fmt.Sprintf("%s: cluster %s has no room for it now", domain.NotAdmittedCapacityUnavailable, cluster)
	case freedInDesiredState && !shortOfADevice(shortage):
		return fmt.Sprintf("%s: cluster %s is short of %s by its last capacity report, and no job the platform has placed there holds it: a job that ended is not reported gone yet, and this one is waiting for the cluster's next capacity report",
			domain.NotAdmittedCapacityUnavailable, cluster, short)
	}
	return fmt.Sprintf("%s: cluster %s is short of %s, %s", domain.NotAdmittedCapacityUnavailable, cluster, short, held)
}

// noNodeOfArchitectureReason is the not_admitted_reason of a job that no node of cluster runs, as
// none names the architecture of its image, and "" when one does. neverFitsReason judges only a
// cluster whose nodes name an architecture, so a job on one whose nodes name none waits with this.
func noNodeOfArchitectureReason(exp *domain.Experiment, cluster string, labelsByNode map[string]map[string]string) string {
	if exp.ImageArchitecture == "" || cluster == "" {
		return ""
	}
	for _, labels := range labelsByNode {
		if exp.OnImageArchitecture(labels) {
			return ""
		}
	}
	nodes := "its nodes name no " + domain.NodeArchitectureLabel
	if has := domain.NodeArchitectures(labelsByNode); len(has) > 0 {
		nodes = "its nodes are of " + strings.Join(has, ", ") + " only"
	}
	return fmt.Sprintf("%s: no node of cluster %s runs the job's image %s, which runs on %s: %s",
		domain.NotAdmittedCapacityUnavailable, cluster, exp.Image, exp.ImageArchitecture, nodes)
}

// apartReason is the not_admitted_reason of a job the free nodes of cluster hold only with its
// nodes in more than one place of its JobSpec.NodesWithin, and "" for any other.
func apartReason(exp *domain.Experiment, cluster string, byNode, nodeResources map[string]map[string]int64, labelsByNode map[string]map[string]string) string {
	if cluster == "" || !placement.FitsOnlyApart(byNode, nodeResources, labelsByNode, exp) {
		return ""
	}
	level := strings.ReplaceAll(string(exp.EffectiveJob().NodesWithin()), "_", " ")
	return fmt.Sprintf("%s: no %s of cluster %s has room for all %d of the job's nodes, which job.topology.nodes_within keeps in one %s",
		domain.NotAdmittedCapacityUnavailable, level, cluster, exp.Job.Nodes(), level)
}

func shortOfADevice(shortage domain.Footprint) bool {
	for key, short := range shortage {
		if short > 0 && key.Kind == domain.ResourceKindAccelerator && !domain.CPUType.Is(domain.AcceleratorType(key.Name)) {
			return true
		}
	}
	return false
}

// holders counts the jobs that hold, on one cluster, what a queued job is short of there: those
// of the queued job's own platform experiment and those of any other. Another platform
// experiment may be another org's, so it is counted and never named.
type holders struct {
	own, others int
}

func (h holders) String() string {
	jobs := func(n int) string {
		if n == 1 {
			return "1 job"
		}
		return fmt.Sprintf("%d jobs", n)
	}
	switch {
	case h.own+h.others == 0:
		return "which no job holds: the cluster's nodes do not have it free"
	case h.others == 0:
		return "held by " + jobs(h.own) + " of this platform experiment"
	case h.own == 0:
		return "held by " + jobs(h.others) + " of other platform experiments"
	}
	return fmt.Sprintf("held by %s: %d of this platform experiment and %d of other platform experiments", jobs(h.own+h.others), h.own, h.others)
}

// holdersOf counts, among the jobs that have claimed capacity, those on cluster that hold some of
// a dimension exp is short of.
func holdersOf(claimed []*domain.Experiment, cluster string, shortage domain.Footprint, exp *domain.Experiment) holders {
	var held holders
	for _, other := range claimed {
		if other.ClusterName != cluster || other.ID == exp.ID {
			continue
		}
		footprint := other.Footprint()
		holds := false
		for key, short := range shortage {
			if short > 0 && footprint[key] > 0 {
				holds = true
				break
			}
		}
		switch {
		case !holds:
		case other.PlatformExperimentID == exp.PlatformExperimentID:
			held.own++
		default:
			held.others++
		}
	}
	return held
}

// describeOffer says what a cluster has installed of what exp asks for, and the most one node of
// it has where it has several. For a job spread over hosts too few of which hold one of its
// nodes, it says how many do.
func describeOffer(total domain.Footprint, nodeTotals map[string]map[string]int64, exp *domain.Experiment) string {
	offer := describeInstalled(total, nodeTotals, exp)
	if held, short := tooFewHosts(nodeTotals, exp); short {
		offer += fmt.Sprintf(", and %d of its %d hosts hold one of the job's nodes", held, len(nodeTotals))
	}
	return offer
}

// tooFewHosts reports whether exp spreads over hosts (placement.RequiresDistinctHosts) and an
// idle cluster whose nodes have nodeTotals has fewer hosts that hold one of its nodes than it has
// nodes, and how many it has.
func tooFewHosts(nodeTotals map[string]map[string]int64, exp *domain.Experiment) (int, bool) {
	if len(nodeTotals) == 0 || !placement.RequiresDistinctHosts(exp) {
		return 0, false
	}
	held := placement.SpreadHostsWhenIdle(nodeTotals, exp)
	return held, held < exp.Job.Nodes()
}

func describeInstalled(total domain.Footprint, nodeTotals map[string]map[string]int64, exp *domain.Experiment) string {
	asked := domain.NewFootprint()
	probe := *exp
	types := placement.CandidateTypes(exp)
	if len(types) == 0 {
		types = []domain.AcceleratorType{exp.AcceleratorType}
	}
	for _, t := range types {
		probe.AcceleratorType = t
		for key := range probe.Footprint() {
			asked[key] = total[key]
		}
	}
	offer := describeFootprint(asked)
	if len(nodeTotals) < 2 {
		return offer
	}
	largest := map[string]int64{}
	for _, node := range nodeTotals {
		for name, amount := range node {
			largest[name] = max(largest[name], amount)
		}
	}
	return offer + ", at most " + describeNodeResources(largest) + " on one node"
}

// ranOnTypeUncoveredReason is the not_admitted_reason of a job that ran on an alternative type
// its quota no longer covers. The type it asked for is not one it may be placed on
// (placement.CandidateTypes), so it waits for quota.
func ranOnTypeUncoveredReason(exp *domain.Experiment) string {
	return fmt.Sprintf("%s: the job ran on %s and is placed on that type alone, which the agent's quota no longer covers", domain.NotAdmittedInsufficientQuota, exp.RanOnAcceleratorType)
}
