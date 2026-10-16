package scheduler

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/scaleresearch/hypothesisloop/controlplane/services/scheduler/placement"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// noOfferReason is the not_admitted_reason of a job that waits for a machine to come on offer:
// no slot of cluster that takes it has room in free, and for one that takes it the runtime's
// vendor has no machine to rent (domain.NodeSlotNoOfferLabel). "" for any other wait.
func noOfferReason(exp *domain.Experiment, cluster string, sizes, free map[string]map[string]int64, labelsByNode map[string]map[string]string) string {
	if _, isFree, misfit := placement.SlotFor(exp, cluster, sizes, free, labelsByNode); misfit != "" || isFree {
		return ""
	}
	why := map[string]bool{}
	for node, labels := range labelsByNode {
		if labels[domain.NodeSlotNoOfferLabel] == "" {
			continue
		}
		if _, _, misfit := placement.SlotFor(exp, cluster, sizes, free, map[string]map[string]string{node: labels}); misfit == "" {
			why[labels[domain.NodeSlotNoOfferLabel]] = true
		}
	}
	if len(why) == 0 {
		return ""
	}
	return fmt.Sprintf("%s: cluster %s has no machine to rent for it now (%s), and the job is admitted when one comes on offer",
		domain.NotAdmittedCapacityUnavailable, cluster, strings.Join(slices.Sorted(maps.Keys(why)), "; "))
}

func waitingForSlotReason(cluster, slot string) string {
	return fmt.Sprintf("%s: waiting for slot %s of cluster %s: no job holds it and its instance has not left yet", domain.NotAdmittedCapacityUnavailable, slot, cluster)
}
