package scheduler

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// Slots are nodes that hold one job each (domain.NodeSlotLabel). Nothing below changes how
// capacity is counted: a job on a slot is resolved to the slot's size, and from there a slot is
// an ordinary node that the job fills. The two rules that are a slot's own are which slot a job
// is sized to (resolveOntoSlot) and that a slot takes only a job of its size (takesWholeSlot).

func isSlot(labels map[string]string) bool { return labels[domain.NodeSlotLabel] != "" }

func hasSlots(labelsByNode map[string]map[string]string) bool {
	for _, labels := range labelsByNode {
		if isSlot(labels) {
			return true
		}
	}
	return false
}

// slotAccelerators is how many accelerators a slot has, taken or free. No label is none.
func slotAccelerators(labels map[string]string) int {
	count, err := strconv.Atoi(labels[domain.NodeSlotAcceleratorsLabel])
	if err != nil {
		return 0
	}
	return count
}

// takesWholeSlot reports whether shape asks for everything a slot has free, given what the slot
// reports free. A slot that is taken reports nothing free and so takes nothing.
func takesWholeSlot(shape domain.NodeShape, nodeKey string, accelerators, resources map[string]int64) bool {
	var free int64
	for _, count := range accelerators {
		free += count
	}
	if free != shape.AcceleratorCount || (shape.AcceleratorCount > 0 && accelerators[nodeKey] != shape.AcceleratorCount) {
		return false
	}
	if len(resources) == 0 {
		return false
	}
	for name, amount := range resources {
		if amount <= 0 || shape.Resources[name] != amount {
			return false
		}
	}
	return true
}

// resolveOntoSlot sizes exp's job to the slot it is to hold on a cluster of slots (slotFor): its
// cpu, memory and storage become that slot's. Which slot of that size the job lands on is decided
// when it is claimed.
func resolveOntoSlot(exp *domain.Experiment, cluster string, sizes, free map[string]map[string]int64, labelsByNode map[string]map[string]string) (resolved *domain.JobSpec, misfit string) {
	node, _, misfit := slotFor(exp, cluster, sizes, free, labelsByNode)
	if misfit != "" {
		return nil, misfit
	}
	size := sizes[node]
	cpu := canonicalQuantityString(domain.NodeResourceCPUMillicores, float64(size[domain.NodeResourceCPUMillicores]))
	memory := canonicalQuantityString(domain.NodeResourceMemoryBytes, float64(size[domain.NodeResourceMemoryBytes]))
	storage := canonicalQuantityString(domain.NodeResourceStorageBytes, float64(size[domain.NodeResourceStorageBytes]))
	out := exp.Job
	if len(exp.Job.Groups) > 0 {
		out.Groups = append([]domain.JobGroup(nil), exp.Job.Groups...)
		out.Groups[0].CPU, out.Groups[0].Memory, out.Groups[0].Storage = cpu, memory, storage
	} else {
		out.CPU, out.Memory, out.Storage = cpu, memory, storage
	}
	return &out, ""
}

// slotFor is the one choice of the slot a job is to hold on a cluster of slots.
//
// sizes is each slot's whole size and free what each has free: now, once the instances that are
// leaving are gone (vacatingSlots), or for a preemption once its candidates are gone. The slot is
// one whose accelerators are exactly the job's, of its type, that carries the job's node selector
// and is at least as large as every quantity the job named. Of those a free one is taken before a
// taken one, and the smallest before a larger: a job that must wait or preempt is sized to the
// least it needs. isFree says which of the two the chosen slot is.
//
// misfit is the not_admitted_reason when no slot of the cluster takes the job.
func slotFor(exp *domain.Experiment, cluster string, sizes, free map[string]map[string]int64, labelsByNode map[string]map[string]string) (node string, isFree bool, misfit string) {
	if exp.Job.Nodes() != 1 {
		return "", false, fmt.Sprintf("%s: cluster %s runs a job on one node", domain.NotAdmittedCapacityUnavailable, cluster)
	}
	group := exp.Job.NodeGroups()[0]
	type slot struct {
		node string
		size map[string]int64
		free bool
	}
	var slots []slot
	for node, labels := range labelsByNode {
		size := sizes[node]
		if !isSlot(labels) || !labelsMatch(labels, exp.Job.NodeSelector) || slotAccelerators(labels) != group.AcceleratorCount {
			continue
		}
		if group.AcceleratorCount > 0 && !exp.AcceleratorType.MatchesLabels(labels) {
			continue
		}
		if size[domain.NodeResourceCPUMillicores] <= 0 || size[domain.NodeResourceMemoryBytes] <= 0 || size[domain.NodeResourceStorageBytes] <= 0 {
			continue
		}
		if !slotCovers(group.CPU, domain.NodeResourceCPUMillicores, size) || !slotCovers(group.Memory, domain.NodeResourceMemoryBytes, size) || !slotCovers(group.Storage, domain.NodeResourceStorageBytes, size) {
			continue
		}
		slots = append(slots, slot{node: node, size: size, free: nodeHasRoom(free[node], size)})
	}
	if len(slots) == 0 {
		return "", false, fmt.Sprintf("%s: no instance shape of cluster %s takes this job: a shape takes a job with exactly its accelerators (%d of %s here) whose cpu, memory and storage fit in it",
			domain.NotAdmittedCapacityUnavailable, cluster, group.AcceleratorCount, exp.AcceleratorType)
	}
	sort.Slice(slots, func(a, b int) bool {
		if slots[a].free != slots[b].free {
			return slots[a].free
		}
		for _, name := range []string{domain.NodeResourceCPUMillicores, domain.NodeResourceMemoryBytes, domain.NodeResourceStorageBytes} {
			if slots[a].size[name] != slots[b].size[name] {
				return slots[a].size[name] < slots[b].size[name]
			}
		}
		return slots[a].node < slots[b].node
	})
	return slots[0].node, slots[0].free, ""
}

// vacatingSlots is the whole size of every slot of cluster that was taken when the tick began and
// that no claimed job holds: the job it had is requeued or over and its instance has not left yet.
// Given to slotFor as what is free, it answers per slot whether a job's capacity is already on
// its way. The cluster total cannot: a slot of another shape covers the job there and never
// takes it.
func vacatingSlots(cluster string, sizes, freeAtTickStart map[string]map[string]int64, labelsByNode map[string]map[string]string, claimed []*domain.Experiment) map[string]map[string]int64 {
	held := map[string]bool{}
	for _, exp := range claimed {
		if exp.ClusterName == cluster {
			held[exp.EffectiveJob().NodeSelector[domain.NodeSlotLabel]] = true
		}
	}
	vacating := map[string]map[string]int64{}
	for node, labels := range labelsByNode {
		if !isSlot(labels) || labels[domain.NodeSlotNoOfferLabel] != "" || held[labels[domain.NodeSlotLabel]] || nodeHasRoom(freeAtTickStart[node], sizes[node]) {
			continue
		}
		vacating[node] = sizes[node]
	}
	return vacating
}

// noOfferReason is the not_admitted_reason of a job that waits for a machine to come on offer:
// no slot of cluster that takes it has room in free, and for one that takes it the runtime's
// vendor has no machine to rent (domain.NodeSlotNoOfferLabel). "" for any other wait.
func noOfferReason(exp *domain.Experiment, cluster string, sizes, free map[string]map[string]int64, labelsByNode map[string]map[string]string) string {
	if _, isFree, misfit := slotFor(exp, cluster, sizes, free, labelsByNode); misfit != "" || isFree {
		return ""
	}
	why := map[string]bool{}
	for node, labels := range labelsByNode {
		if labels[domain.NodeSlotNoOfferLabel] == "" {
			continue
		}
		if _, _, misfit := slotFor(exp, cluster, sizes, free, map[string]map[string]string{node: labels}); misfit == "" {
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

// slotCovers reports whether a slot of this size holds the quantity a job named.
func slotCovers(quantity, name string, size map[string]int64) bool {
	value, err := canonicalQuantityValue(name, quantity)
	return err == nil && value <= float64(size[name])
}

// onSlots returns exp as it would be admitted to a cluster, for a fit check made before the tick
// has resolved it there: sized to a free slot when the cluster is one of slots, exp itself
// otherwise. ok is false when the cluster is one of slots and none takes the job.
func onSlots(exp *domain.Experiment, cluster string, free map[string]map[string]int64, labelsByNode map[string]map[string]string) (probe *domain.Experiment, ok bool) {
	if !hasSlots(labelsByNode) {
		return exp, true
	}
	resolved, misfit := resolveOntoSlot(exp, cluster, free, free, labelsByNode)
	if misfit != "" {
		return nil, false
	}
	sized := *exp
	sized.ResolvedJob = resolved
	return &sized, true
}

// holdSlot marks the slot a desired job is pinned to as taken.
func holdSlot(byNode, nodeResources map[string]map[string]int64, labelsByNode map[string]map[string]string, slot string) {
	for node, labels := range labelsByNode {
		if labels[domain.NodeSlotLabel] != slot {
			continue
		}
		for key := range byNode[node] {
			byNode[node][key] = 0
		}
		for key := range nodeResources[node] {
			nodeResources[node][key] = 0
		}
	}
}
