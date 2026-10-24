package scheduler

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/scheduler/placement"
)

// slotWaitReason is the not_admitted_reason of a job that waits for what the runtime of a cluster
// of slots has to rent: a private network for a job that spans nodes (noNetworkReason), or a
// machine on offer (noOfferReason). "" for any other wait.
func slotWaitReason(exp *domain.Experiment, cluster string, sizes, free map[string]map[string]int64, labelsByNode map[string]map[string]string) string {
	if reason := noNetworkReason(exp, cluster, sizes, free, labelsByNode); reason != "" {
		return reason
	}
	return noOfferReason(exp, cluster, sizes, free, labelsByNode)
}

// noNetworkReason is the not_admitted_reason of a job that spans nodes and waits for a private
// network: the slots it is to hold are free, and no network of the runtime's holds them now
// (placement.NetworkWait). "" for any other wait.
func noNetworkReason(exp *domain.Experiment, cluster string, sizes, free map[string]map[string]int64, labelsByNode map[string]map[string]string) string {
	ranks, shape, why, ok := placement.NetworkWait(exp, cluster, sizes, free, labelsByNode)
	if !ok {
		return ""
	}
	reason := fmt.Sprintf("%s: cluster %s has no private network for %d ranks of shape %q now", domain.NotAdmittedCapacityUnavailable, cluster, ranks, shape)
	if len(why) > 0 {
		reason += " (" + strings.Join(why, "; ") + ")"
	}
	return reason
}

// noOfferReason is the not_admitted_reason of a job that waits for a machine to come on offer:
// the slots of cluster that take it have no room in free for it, and for one that takes it the
// runtime's vendor has no machine to rent (domain.NodeSlotNoOfferLabel). "" for any other wait.
func noOfferReason(exp *domain.Experiment, cluster string, sizes, free map[string]map[string]int64, labelsByNode map[string]map[string]string) string {
	if _, isFree, misfit := placement.SlotsFor(exp, cluster, sizes, free, labelsByNode); misfit != "" || isFree {
		return ""
	}
	why := map[string]bool{}
	for node, labels := range labelsByNode {
		if labels[domain.NodeSlotNoOfferLabel] != "" && placement.TakesSlot(exp, labels, sizes[node]) {
			why[labels[domain.NodeSlotNoOfferLabel]] = true
		}
	}
	if len(why) == 0 {
		return ""
	}
	return fmt.Sprintf("%s: cluster %s has no machine to rent for it now (%s), and the job is admitted when one comes on offer",
		domain.NotAdmittedCapacityUnavailable, cluster, strings.Join(slices.Sorted(maps.Keys(why)), "; "))
}

func waitingForSlotsReason(cluster string, slots []string) string {
	what, held := "slot "+slots[0], "it and its instance has"
	if len(slots) > 1 {
		what, held = "slots "+strings.Join(slots, ", "), "them and their instances have"
	}
	return fmt.Sprintf("%s: waiting for %s of cluster %s: no job holds %s not left yet", domain.NotAdmittedCapacityUnavailable, what, cluster, held)
}
