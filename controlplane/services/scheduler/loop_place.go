package scheduler

import (
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/services/scheduler/placement"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
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
