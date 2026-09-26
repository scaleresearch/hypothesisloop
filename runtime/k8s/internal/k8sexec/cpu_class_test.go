package k8sexec

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

const testCPUClass = "epyc-9354"

// milliNode is a ready node whose allocatable cpu is given in millicores.
func milliNode(name string, cpuMilli int64) *corev1.Node {
	node := totalCapacityTestNode(name, 0, 64)
	node.Status.Allocatable[corev1.ResourceCPU] = *resource.NewMilliQuantity(cpuMilli, resource.DecimalSI)
	node.Status.Capacity = corev1.ResourceList{corev1.ResourceCPU: *resource.NewQuantity(16, resource.DecimalSI)}
	return node
}

func writes(kube *fake.Clientset) int {
	n := 0
	for _, action := range kube.Actions() {
		if action.GetVerb() == "patch" {
			n++
		}
	}
	return n
}

func units(t *testing.T, kube *fake.Clientset, name string) (capacity, allocatable int64, class string) {
	t.Helper()
	node, err := kube.CoreV1().Nodes().Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	c, a := node.Status.Capacity[CPUClassResource], node.Status.Allocatable[CPUClassResource]
	return c.Value(), a.Value(), node.Labels[CPUClassLabel]
}

// The units a node offers are its whole allocatable cores: what jobs can hold, never the
// machine's core count and never a round number above it.
func TestANodeOffersOneUnitOfTheCPUClassPerWholeAllocatableCore(t *testing.T) {
	cordoned := milliNode("cordoned", 16000)
	cordoned.Spec.Unschedulable = true
	notReady := milliNode("not-ready", 16000)
	notReady.Status.Conditions[0].Status = corev1.ConditionFalse
	kube := fake.NewClientset(milliNode("box", 15800), milliNode("small", 3999), milliNode("exact", 8000), milliNode("tiny", 900), cordoned, notReady)
	c := &JobWorkloadClient{kube: kube, cpuClass: testCPUClass}

	if err := c.PublishCPUClass(context.Background()); err != nil {
		t.Fatal(err)
	}

	for name, want := range map[string]int64{"box": 15, "small": 3, "exact": 8, "tiny": 0} {
		capacity, allocatable, class := units(t, kube, name)
		if capacity != want || allocatable != want || class != testCPUClass {
			t.Errorf("%s: capacity %d, allocatable %d, class %q; want %d units of %s", name, capacity, allocatable, class, want, testCPUClass)
		}
	}
	for _, name := range []string{"cordoned", "not-ready"} {
		if capacity, allocatable, class := units(t, kube, name); capacity != 0 || allocatable != 0 || class != "" {
			t.Errorf("%s takes no job and was given %d units of %q", name, capacity, class)
		}
	}
	// The node's own cores and memory are as they were.
	node, _ := kube.CoreV1().Nodes().Get(context.Background(), "box", metav1.GetOptions{})
	if cpu := node.Status.Allocatable[corev1.ResourceCPU]; cpu.MilliValue() != 15800 {
		t.Errorf("publishing changed the node's allocatable cpu to %s", cpu.String())
	}
}

func TestPublishingAgainWritesOnlyWhatChanged(t *testing.T) {
	kube := fake.NewClientset(milliNode("box", 15800), milliNode("other", 8000))
	c := &JobWorkloadClient{kube: kube, cpuClass: testCPUClass}
	ctx := context.Background()
	if err := c.PublishCPUClass(ctx); err != nil {
		t.Fatal(err)
	}
	first := writes(kube)
	if first != 4 {
		t.Fatalf("the first publish made %d writes, want the units and the label of each of two nodes", first)
	}
	if err := c.PublishCPUClass(ctx); err != nil || writes(kube) != first {
		t.Fatalf("publishing an unchanged cluster again: err = %v, writes = %d, want none beyond %d", err, writes(kube), first)
	}

	// The node shrinks, and a node joins.
	node, _ := kube.CoreV1().Nodes().Get(ctx, "box", metav1.GetOptions{})
	node.Status.Allocatable[corev1.ResourceCPU] = *resource.NewMilliQuantity(11500, resource.DecimalSI)
	if _, err := kube.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := kube.CoreV1().Nodes().Create(ctx, milliNode("joined", 4000), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := c.PublishCPUClass(ctx); err != nil {
		t.Fatal(err)
	}
	if got := writes(kube); got != first+3 {
		t.Errorf("after one node shrank and one joined: %d writes, want 3 more than %d", got, first)
	}
	if capacity, allocatable, _ := units(t, kube, "box"); capacity != 11 || allocatable != 11 {
		t.Errorf("box offers %d/%d units of its 11.5 cores, want 11", capacity, allocatable)
	}
	if capacity, _, class := units(t, kube, "joined"); capacity != 4 || class != testCPUClass {
		t.Errorf("the node that joined offers %d units of %q, want 4 of the class", capacity, class)
	}

	// A class renamed in the configuration renames the label and leaves the units.
	renamed := &JobWorkloadClient{kube: kube, cpuClass: "epyc-genoa"}
	before := writes(kube)
	if err := renamed.PublishCPUClass(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, class := units(t, kube, "box"); class != "epyc-genoa" || writes(kube) != before+3 {
		t.Errorf("after a rename: class %q with %d writes, want the new label on three nodes and nothing else", class, writes(kube)-before)
	}
}

func TestAClusterWithNoCPUClassWritesNoNode(t *testing.T) {
	kube := fake.NewClientset(milliNode("box", 15800))
	if err := (&JobWorkloadClient{kube: kube}).PublishCPUClass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(kube.Actions()) != 0 {
		t.Errorf("a cluster with no CPU class configured made %d API calls", len(kube.Actions()))
	}
	if capacity, _, class := units(t, kube, "box"); capacity != 0 || class != "" {
		t.Errorf("the node was given %d units of %q", capacity, class)
	}
}

func TestACPUClassIsANodeLabelValue(t *testing.T) {
	for _, name := range []string{"", "epyc-9354", "Xeon_8480.v2"} {
		if err := validCPUClass(name); err != nil {
			t.Errorf("%q: %v", name, err)
		}
	}
	for _, name := range []string{"epyc 9354", "cpu.hypothesisloop.dev/class=epyc", "-epyc", "a/b"} {
		if err := validCPUClass(name); err == nil {
			t.Errorf("%q was accepted", name)
		}
	}
}

// Once published, the class is a type like a device-plugin accelerator to everything that reads
// the cluster: capacity lists it under the string a job names, less what pods hold, and a job on
// it is placed by the resource and the label.
func TestAPublishedCPUClassIsReportedAndRequestedLikeAnAccelerator(t *testing.T) {
	const flavor = CPUClassLabel + "=" + testCPUClass
	holder := requestingPod("holder", "box", true, 4000)
	holder.Spec.Containers[0].Resources.Requests[CPUClassResource] = *resource.NewQuantity(4, resource.DecimalSI)
	kube := fake.NewClientset(milliNode("box", 15800), milliNode("other", 8000), holder)
	c := newDRATestClient()
	c.kube, c.cpuClass = kube, testCPUClass
	ctx := context.Background()
	if err := c.PublishCPUClass(ctx); err != nil {
		t.Fatal(err)
	}

	available, total, byNode, _, err := c.GetLiveAcceleratorCapacitySnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if total[flavor] != 23 || available[flavor] != 19 || byNode["box"][flavor] != 11 || byNode["other"][flavor] != 8 {
		t.Errorf("capacity of %s: total %d, available %d, by node %v; want 23 in all, 19 free, 11 on box and 8 on other", flavor, total[flavor], available[flavor], byNode)
	}
	if len(total) != 1 {
		t.Errorf("capacity lists %v, want the class alone", total)
	}

	placement, found, err := c.resolveDevicePluginPlacement(ctx, domain.AcceleratorType(flavor))
	if err != nil || !found {
		t.Fatalf("placement of %s: found = %v, err = %v", flavor, found, err)
	}
	want := AcceleratorPlacement{ResourceName: string(CPUClassResource), NodeLabelKey: CPUClassLabel, NodeLabelValue: testCPUClass}
	if placement != want {
		t.Errorf("placement = %+v, want %+v", placement, want)
	}
}
