package placement

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// Slots are nodes that hold one job each (domain.NodeSlotLabel). Nothing below changes how
// capacity is counted: a job on a slot is resolved to the slot's size, and from there a slot is
// an ordinary node that the job fills. The rules that are a slot's own are which slots a job is
// sized to (ResolveOntoSlots), that a slot takes only a node of its size (takesWholeSlot), and that
// the nodes of a job that spans several share one private network (SlotNetwork).

// onCores says exp holds cores and no device (domain.CPUType).
func onCores(exp *domain.Experiment) bool { return exp.AcceleratorType.Kind() == domain.TypeKindCPU }

func isSlot(labels map[string]string) bool { return labels[domain.NodeSlotLabel] != "" }

func HasSlots(labelsByNode map[string]map[string]string) bool {
	for _, labels := range labelsByNode {
		if isSlot(labels) {
			return true
		}
	}
	return false
}

// SlotAccelerators is how many accelerators a slot has, taken or free. No label is none.
func SlotAccelerators(labels map[string]string) int {
	return labelCount(labels, domain.NodeSlotAcceleratorsLabel)
}

// SlotNetwork is the most nodes of a free slot's shape that one private network of the runtime's
// holds now (domain.NodeSlotNetworkLabel). No label is none.
func SlotNetwork(labels map[string]string) int {
	return labelCount(labels, domain.NodeSlotNetworkLabel)
}

func labelCount(labels map[string]string, key string) int {
	count, err := strconv.Atoi(labels[key])
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

// ResolveOntoSlots sizes exp's job to the slots it is to hold on a cluster of slots (SlotsFor):
// each group's cpu, memory and storage become those of its slots' shape, and for a job on CPU its
// count too, which is its cores. Which slots of those shapes the job lands on is decided when it is
// claimed (DesiredFits).
func ResolveOntoSlots(exp *domain.Experiment, cluster string, sizes, free map[string]map[string]int64, labelsByNode map[string]map[string]string) (resolved *domain.JobSpec, misfit string) {
	nodes, _, misfit := SlotsFor(exp, cluster, sizes, free, labelsByNode)
	if misfit != "" {
		return nil, misfit
	}
	out := exp.Job
	if len(exp.Job.Groups) == 0 {
		out.CPU, out.Memory, out.Storage, out.AcceleratorCount = sizedTo(exp, sizes[nodes[0]], out.AcceleratorCount)
		return &out, ""
	}
	out.Groups = append([]domain.JobGroup(nil), exp.Job.Groups...)
	rank := 0
	for i := range out.Groups {
		g := &out.Groups[i]
		g.CPU, g.Memory, g.Storage, g.AcceleratorCount = sizedTo(exp, sizes[nodes[rank]], g.AcceleratorCount)
		rank += g.Replicas
	}
	return &out, ""
}

// sizedTo is a node of exp sized to a slot of size: its quantities, and its count, which for a job
// on CPU is the slot's cores and otherwise stays count.
func sizedTo(exp *domain.Experiment, size map[string]int64, count int) (cpu, memory, storage string, accelerators int) {
	cpu = QuantityString(domain.NodeResourceCPUMillicores, float64(size[domain.NodeResourceCPUMillicores]))
	memory = QuantityString(domain.NodeResourceMemoryBytes, float64(size[domain.NodeResourceMemoryBytes]))
	storage = QuantityString(domain.NodeResourceStorageBytes, float64(size[domain.NodeResourceStorageBytes]))
	if onCores(exp) {
		count = int(size[domain.NodeResourceCPUMillicores] / 1000)
	}
	return cpu, memory, storage, count
}

// SlotsFor is the one choice of the slots a job is to hold on a cluster of slots, one per node of
// the job in rank order.
//
// sizes is each slot's whole size and free what each has free: now, once the instances that are
// leaving are gone (VacatingSlots), or for a preemption once its candidates are gone. Each group
// of the job holds slots of one shape (shapeOf), a shape whose slots take the group (takes), and
// every group of a job that spans nodes the same one (oneShape). Of
// the shapes, one with free slots for the whole group is taken before one without, and the
// smallest before a larger: a job that must wait or preempt is sized to the least it needs. Of a
// shape's slots the free ones go first. A slot of a job that spans nodes is free only while one
// private network of the runtime's holds every node of the job on its shape (SlotNetwork). isFree
// says whether every chosen slot is.
//
// misfit is the not_admitted_reason when the slots of the cluster cannot take the job.
func SlotsFor(exp *domain.Experiment, cluster string, sizes, free map[string]map[string]int64, labelsByNode map[string]map[string]string) (nodes []string, isFree bool, misfit string) {
	return slotsFor(exp, cluster, sizes, free, labelsByNode, exp.Job.Nodes() > 1)
}

// slotsFor is SlotsFor, with the private network asked of a free slot only when networked.
func slotsFor(exp *domain.Experiment, cluster string, sizes, free map[string]map[string]int64, labelsByNode map[string]map[string]string, networked bool) (nodes []string, isFree bool, misfit string) {
	type slot struct {
		node string
		free bool
	}
	type option struct {
		shape string
		size  map[string]int64
		slots []slot
		free  bool
	}
	one, misfit := oneShape(exp, cluster, sizes, labelsByNode)
	if misfit != "" {
		return nil, false, misfit
	}
	chosen := map[string]bool{}
	onShape := map[string]int{}
	for _, group := range exp.Job.NodeGroups() {
		byShape := map[string]*option{}
		for node, labels := range labelsByNode {
			if chosen[node] || !takes(exp, group, labels, sizes[node]) {
				continue
			}
			shape := shapeOf(labels, sizes[node])
			if one != nil && !one[shape] {
				continue
			}
			o := byShape[shape]
			if o == nil {
				o = &option{shape: shape, size: sizes[node]}
				byShape[shape] = o
			}
			o.slots = append(o.slots, slot{node: node, free: NodeHasRoom(free[node], sizes[node]) && (!networked || SlotNetwork(labels) >= onShape[shape]+group.Replicas)})
		}
		if len(byShape) == 0 {
			return nil, false, fmt.Sprintf("%s: no instance shape of cluster %s takes this job: a shape takes a job with exactly its accelerators (%d of %s here, none for a job on CPU) whose cpu, memory and storage fit in it",
				domain.NotAdmittedCapacityUnavailable, cluster, slotDevices(exp, group), exp.AcceleratorType)
		}
		var options []*option
		for _, o := range byShape {
			if len(o.slots) < group.Replicas {
				continue
			}
			sort.Slice(o.slots, func(a, b int) bool {
				if o.slots[a].free != o.slots[b].free {
					return o.slots[a].free
				}
				return o.slots[a].node < o.slots[b].node
			})
			o.free = o.slots[group.Replicas-1].free
			options = append(options, o)
		}
		if len(options) == 0 {
			return nil, false, fmt.Sprintf("%s: no instance shape of cluster %s has %d slots that take %s", domain.NotAdmittedCapacityUnavailable, cluster, group.Replicas, groupOf(group))
		}
		sort.Slice(options, func(a, b int) bool {
			if options[a].free != options[b].free {
				return options[a].free
			}
			for _, name := range []string{domain.NodeResourceCPUMillicores, domain.NodeResourceMemoryBytes, domain.NodeResourceStorageBytes} {
				if options[a].size[name] != options[b].size[name] {
					return options[a].size[name] < options[b].size[name]
				}
			}
			return options[a].shape < options[b].shape
		})
		for _, s := range options[0].slots[:group.Replicas] {
			chosen[s.node] = true
			nodes = append(nodes, s.node)
		}
		onShape[options[0].shape] += group.Replicas
	}
	isFree = true
	for _, node := range nodes {
		labels := labelsByNode[node]
		if !NodeHasRoom(free[node], sizes[node]) || (networked && SlotNetwork(labels) < onShape[shapeOf(labels, sizes[node])]) {
			isFree = false
		}
	}
	return nodes, isFree, ""
}

// oneShape is the shapes every group of a job that spans nodes in several groups could hold, nil
// for any other job. The nodes of a job meet on one private network, which the runtime makes for
// one shape: two shapes need not be rentable in one place, so a job admitted to both would wait
// for a network and go back to the queue without end. misfit names the shapes each group takes
// when no shape takes them all.
func oneShape(exp *domain.Experiment, cluster string, sizes map[string]map[string]int64, labelsByNode map[string]map[string]string) (map[string]bool, string) {
	groups := exp.Job.NodeGroups()
	if exp.Job.Nodes() <= 1 || len(groups) <= 1 {
		return nil, ""
	}
	taking := make([]map[string]string, len(groups))
	for g, group := range groups {
		taking[g] = map[string]string{}
		for node, labels := range labelsByNode {
			if takes(exp, group, labels, sizes[node]) {
				taking[g][shapeOf(labels, sizes[node])] = describeShape(exp, labels, sizes[node])
			}
		}
		if len(taking[g]) == 0 {
			return nil, ""
		}
	}
	one := map[string]bool{}
	for shape := range taking[0] {
		if !slices.ContainsFunc(taking[1:], func(t map[string]string) bool { _, ok := t[shape]; return !ok }) {
			one[shape] = true
		}
	}
	if len(one) > 0 {
		return one, ""
	}
	described := make([]string, len(groups))
	for g, group := range groups {
		described[g] = fmt.Sprintf("%s takes %s", groupOf(group), strings.Join(slices.Sorted(maps.Values(taking[g])), " or "))
	}
	return nil, fmt.Sprintf("%s: the nodes of a job that spans several on cluster %s meet on one private network, which holds instances of one shape, and no shape of the cluster takes every group: %s",
		domain.NotAdmittedCapacityUnavailable, cluster, strings.Join(described, "; "))
}

// NetworkWait is what a job that spans nodes waits for when the slots it is to hold are free and
// no private network of the runtime's holds them now: ranks of its nodes on slots of shape, and
// the runtime's reasons (domain.NodeSlotNoNetworkLabel). ok is false for a job that waits for
// anything else, or for nothing.
func NetworkWait(exp *domain.Experiment, cluster string, sizes, free map[string]map[string]int64, labelsByNode map[string]map[string]string) (ranks int, shape string, why []string, ok bool) {
	if exp.Job.Nodes() <= 1 {
		return 0, "", nil, false
	}
	if _, isFree, misfit := slotsFor(exp, cluster, sizes, free, labelsByNode, true); misfit != "" || isFree {
		return 0, "", nil, false
	}
	nodes, isFree, misfit := slotsFor(exp, cluster, sizes, free, labelsByNode, false)
	if misfit != "" || !isFree {
		return 0, "", nil, false
	}
	onShape := map[string]int{}
	for _, node := range nodes {
		onShape[shapeOf(labelsByNode[node], sizes[node])]++
	}
	for _, node := range nodes {
		labels := labelsByNode[node]
		key := shapeOf(labels, sizes[node])
		if SlotNetwork(labels) >= onShape[key] {
			continue
		}
		reasons := map[string]bool{}
		for other, otherLabels := range labelsByNode {
			if why := otherLabels[domain.NodeSlotNoNetworkLabel]; why != "" && shapeOf(otherLabels, sizes[other]) == key {
				reasons[why] = true
			}
		}
		return onShape[key], describeShape(exp, labels, sizes[node]), slices.Sorted(maps.Keys(reasons)), true
	}
	return 0, "", nil, false
}

// TakesSlot says whether a node of exp could hold a slot of these labels and this size.
func TakesSlot(exp *domain.Experiment, labels map[string]string, size map[string]int64) bool {
	return slices.ContainsFunc(exp.Job.NodeGroups(), func(group domain.JobGroup) bool { return takes(exp, group, labels, size) })
}

// takes says whether a slot of these labels and this size takes a node of group: its accelerators
// are exactly the group's, of the job's type, it carries the job's node selector and it is at
// least as large as every quantity the group named. A job on CPU counts its cores and holds no
// device, so its slot is one with no accelerators.
func takes(exp *domain.Experiment, group domain.JobGroup, labels map[string]string, size map[string]int64) bool {
	devices := slotDevices(exp, group)
	if !isSlot(labels) || !RunsOn(labels, exp) || SlotAccelerators(labels) != devices {
		return false
	}
	if devices > 0 && !exp.AcceleratorType.MatchesLabels(labels) {
		return false
	}
	if size[domain.NodeResourceCPUMillicores] <= 0 || size[domain.NodeResourceMemoryBytes] <= 0 || size[domain.NodeResourceStorageBytes] <= 0 {
		return false
	}
	return slotCovers(group.CPU, domain.NodeResourceCPUMillicores, size) && slotCovers(group.Memory, domain.NodeResourceMemoryBytes, size) && slotCovers(group.Storage, domain.NodeResourceStorageBytes, size)
}

// slotDevices is the accelerators a slot that takes a node of group has.
func slotDevices(exp *domain.Experiment, group domain.JobGroup) int {
	if onCores(exp) {
		return 0
	}
	return group.AcceleratorCount
}

// shapeOf names the shape of a slot: its accelerators and its size. Slots of one shape are alike
// to every job, and a job sized to one fills any of them.
func shapeOf(labels map[string]string, size map[string]int64) string {
	return fmt.Sprintf("%d/%d/%d/%d", SlotAccelerators(labels), size[domain.NodeResourceCPUMillicores], size[domain.NodeResourceMemoryBytes], size[domain.NodeResourceStorageBytes])
}

// describeShape is the shape of a slot of exp's in words.
func describeShape(exp *domain.Experiment, labels map[string]string, size map[string]int64) string {
	described := fmt.Sprintf("%s cpu, %s memory, %s storage",
		QuantityString(domain.NodeResourceCPUMillicores, float64(size[domain.NodeResourceCPUMillicores])),
		QuantityString(domain.NodeResourceMemoryBytes, float64(size[domain.NodeResourceMemoryBytes])),
		QuantityString(domain.NodeResourceStorageBytes, float64(size[domain.NodeResourceStorageBytes])))
	if devices := SlotAccelerators(labels); devices > 0 {
		described = fmt.Sprintf("%d of %s, %s", devices, exp.AcceleratorType, described)
	}
	return described
}

// groupOf names group in a reason.
func groupOf(group domain.JobGroup) string {
	if group.Name == "" {
		return "the job's nodes"
	}
	return fmt.Sprintf("group %q of the job", group.Name)
}

// VacatingSlots is the whole size of every slot of cluster that was taken when the tick began and
// that no claimed job holds: the job it had is requeued or over and its instance has not left yet.
// Given to SlotsFor as what is free, it answers per slot whether a job's capacity is already on
// its way. The cluster total cannot: a slot of another shape covers the job there and never
// takes it.
func VacatingSlots(cluster string, sizes, freeAtTickStart map[string]map[string]int64, labelsByNode map[string]map[string]string, claimed []*domain.Experiment) map[string]map[string]int64 {
	held := map[string]bool{}
	for _, exp := range claimed {
		if exp.ClusterName == cluster {
			for _, slot := range exp.EffectiveJob().Slots {
				held[slot] = true
			}
		}
	}
	vacating := map[string]map[string]int64{}
	for node, labels := range labelsByNode {
		if !isSlot(labels) || labels[domain.NodeSlotNoOfferLabel] != "" || held[labels[domain.NodeSlotLabel]] || NodeHasRoom(freeAtTickStart[node], sizes[node]) {
			continue
		}
		vacating[node] = sizes[node]
	}
	return vacating
}

// slotCovers reports whether a slot of this size holds the quantity a job named.
func slotCovers(quantity, name string, size map[string]int64) bool {
	value, err := QuantityValue(name, quantity)
	return err == nil && value <= float64(size[name])
}

// OnSlots returns exp as it would be admitted to a cluster, for a fit check made before the tick
// has resolved it there: sized to a free slot when the cluster is one of slots, exp itself
// otherwise. ok is false when the cluster is one of slots and none takes the job.
func OnSlots(exp *domain.Experiment, cluster string, free map[string]map[string]int64, labelsByNode map[string]map[string]string) (probe *domain.Experiment, ok bool) {
	if !HasSlots(labelsByNode) {
		return exp, true
	}
	resolved, misfit := ResolveOntoSlots(exp, cluster, free, free, labelsByNode)
	if misfit != "" {
		return nil, false
	}
	sized := *exp
	sized.ResolvedJob = resolved
	return &sized, true
}

// HoldSlots marks the slots a desired job is pinned to as taken.
func HoldSlots(byNode, nodeResources map[string]map[string]int64, labelsByNode map[string]map[string]string, slots []string) {
	for node, labels := range labelsByNode {
		if !isSlot(labels) || !slices.Contains(slots, labels[domain.NodeSlotLabel]) {
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
