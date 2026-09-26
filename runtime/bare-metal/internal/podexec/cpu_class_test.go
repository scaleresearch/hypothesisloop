package podexec

import (
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/workloadkeys"
)

// A node that offers its cores as a CPU class beside one chip: 32 cores and one device, the
// shape of the machine this was built for. A job on cores and a job on the chip share the node,
// each counted by the units of its own type and both by the cores they hold.

const (
	fakeCPUClass     = "box-cpu"
	fakeCPUClassType = workloadkeys.CPUClassLabel + "=" + fakeCPUClass
)

// classedExecutor is a node of the given cores and devices that offers its cores as fakeCPUClass.
func classedExecutor(t *testing.T, f *fakeEngine, cores int, devicePaths ...string) *Executor {
	t.Helper()
	previous := totalCPUCores
	totalCPUCores = func() float64 { return float64(cores) }
	t.Cleanup(func() { totalCPUCores = previous })
	e := newFakeExecutor(f, devicePaths...)
	e.cpuClass, e.nodeName = fakeCPUClass, "box"
	return e
}

// coreJob asks for cores of the class, as submission leaves such a job: its cpu is its count.
func coreJob(id string, cores int) *domain.Experiment {
	exp := fakeJob(id, "fast", cores)
	exp.AcceleratorType, exp.Job.AcceleratorType = fakeCPUClassType, fakeCPUClassType
	exp.Job.CPU = strconv.Itoa(cores)
	return exp
}

func chipJob(id, cpu string) *domain.Experiment {
	exp := fakeJob(id, "fast", 1)
	exp.Job.CPU = cpu
	return exp
}

type nodeCapacity struct {
	Available, Total map[string]int64
	FreeCores        float64
}

func capacityOf(t *testing.T, e *Executor) nodeCapacity {
	t.Helper()
	available, total, byNode, labels, err := e.GetLiveAcceleratorCapacitySnapshot(t.Context())
	if err != nil {
		t.Fatalf("GetLiveAcceleratorCapacitySnapshot: %v", err)
	}
	if !reflect.DeepEqual(byNode, map[string]map[string]int64{e.nodeName: available}) {
		t.Fatalf("by node = %v, want the one node holding %v", byNode, available)
	}
	if got := labels[e.nodeName][workloadkeys.CPUClassLabel]; got != e.cpuClass {
		t.Fatalf("the node's %s label = %q, want %q", workloadkeys.CPUClassLabel, got, e.cpuClass)
	}
	free, _, err := e.GetLiveCPUCapacity(t.Context())
	if err != nil {
		t.Fatalf("GetLiveCPUCapacity: %v", err)
	}
	return nodeCapacity{available, total, free}
}

func running(f *fakeEngine, ids ...string) func() bool {
	return func() bool {
		for _, id := range ids {
			if f.state(id) != "running" {
				return false
			}
		}
		return true
	}
}

func TestANodeOffersItsWholeCoresAsACPUClassBesideItsChip(t *testing.T) {
	f := newFakeEngine()
	e := classedExecutor(t, f, 32, "/dev/fake/0")

	got := capacityOf(t, e)

	want := nodeCapacity{
		Available: map[string]int64{fakeAcceleratorType: 1, fakeCPUClassType: 32},
		Total:     map[string]int64{fakeAcceleratorType: 1, fakeCPUClassType: 32},
		FreeCores: 32,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("capacity = %+v, want %+v", got, want)
	}
}

func TestACoreJobAndAChipJobShareTheNode(t *testing.T) {
	f := newFakeEngine()
	e := classedExecutor(t, f, 32, "/dev/fake/0")
	ctx := t.Context()
	cores, chip := coreJob("cores", 4), chipJob("chip", "8")

	pass(t, ctx, e, cores, chip)
	eventually(t, "both jobs run", running(f, "cores", "chip"))
	eventually(t, "their starts have returned", func() bool { return len(e.operations()) == 0 })
	// A second pass finds both as desired: neither is moved, recreated or reported drifted.
	pass(t, ctx, e, cores, chip)

	if got := f.devices("cores"); len(got) != 0 {
		t.Errorf("the job on cores was handed %v", got)
	}
	if got := f.devices("chip"); !reflect.DeepEqual(got, []string{"/dev/fake/0"}) {
		t.Errorf("the job on the chip holds %v, want the chip", got)
	}
	if got := f.containers[containerName("cores")].host.NanoCPUs; got != 4_000_000_000 {
		t.Errorf("the job on 4 cores runs under a cpu limit of %d nanocores, want 4 cores", got)
	}
	want := nodeCapacity{
		Available: map[string]int64{fakeAcceleratorType: 0, fakeCPUClassType: 28},
		Total:     map[string]int64{fakeAcceleratorType: 1, fakeCPUClassType: 32},
		FreeCores: 20,
	}
	if got := capacityOf(t, e); !reflect.DeepEqual(got, want) {
		t.Fatalf("capacity = %+v, want %+v: 4 units and 4 cores to the core job, the chip and 8 cores to the chip job", got, want)
	}
}

// The chip is a device and the cores are a limit. A job on cores never takes the chip, however
// many cores it asks for, and jobs on cores start while the chip is held.
func TestJobsOnCoresNeverHoldTheChip(t *testing.T) {
	f := newFakeEngine()
	e := classedExecutor(t, f, 32, "/dev/fake/0")
	ctx := t.Context()
	a, b, big := coreJob("a", 1), coreJob("b", 8), coreJob("big", 16)
	first, second := chipJob("first", "2"), chipJob("second", "2")

	pass(t, ctx, e, a, b, big, first)
	eventually(t, "the three core jobs and the chip job run", running(f, "a", "b", "big", "first"))
	eventually(t, "their starts have returned", func() bool { return len(e.operations()) == 0 })
	pass(t, ctx, e, a, b, big, first, second)

	if got := f.state("second"); got != "" {
		t.Fatalf("a second chip job got a %s container on a node with one chip", got)
	}
	for _, id := range []string{"a", "b", "big"} {
		if got := f.devices(id); len(got) != 0 {
			t.Errorf("core job %s holds %v", id, got)
		}
	}
	if len(f.doubleBooked) > 0 {
		t.Fatalf("device handed out twice: %v", f.doubleBooked)
	}
	if got := capacityOf(t, e); got.Available[fakeCPUClassType] != 7 || got.Available[fakeAcceleratorType] != 0 || got.FreeCores != 5 {
		t.Fatalf("capacity = %+v, want 7 units, no chip and 5 cores free", got)
	}

	// The chip frees and the second chip job takes it; the core jobs are where they were.
	pass(t, ctx, e, a, b, big, second)
	eventually(t, "the first chip job's stop has returned", idle(e, "first"))
	pass(t, ctx, e, a, b, big, second)
	eventually(t, "the second chip job runs", running(f, "second"))
	eventually(t, "its start has returned", func() bool { return len(e.operations()) == 0 })
	pass(t, ctx, e, a, b, big, second)
	if got := f.devices("second"); !reflect.DeepEqual(got, []string{"/dev/fake/0"}) {
		t.Fatalf("the second chip job holds %v, want the chip the first gave up", got)
	}
}

// Units are held like devices: from before a job's pull starts until its stop returns, so the
// class never reads freer than the node is.
func TestCoreUnitsAreHeldThroughAPullAndAStopGrace(t *testing.T) {
	f := newFakeEngine()
	e := classedExecutor(t, f, 8)
	ctx := t.Context()
	releasePull := f.block("pull slow")
	defer releasePull()
	pulling := coreJob("pulling", 3)
	pulling.Job.Image = "slow"
	stopping := coreJob("stopping", 2)

	pass(t, ctx, e, pulling, stopping)
	eventually(t, "the second job runs beside the pull", running(f, "stopping"))
	eventually(t, "its start has returned", idle(e, "stopping"))
	if got := capacityOf(t, e); got.Available[fakeCPUClassType] != 3 || got.FreeCores != 3 {
		t.Fatalf("capacity = %+v, want 3 of 8 units and cores free with 3 pulling and 2 running", got)
	}

	releaseStop := f.block("stop " + containerName("stopping"))
	defer releaseStop()
	pass(t, ctx, e, pulling)
	if got := capacityOf(t, e); got.Available[fakeCPUClassType] != 3 {
		t.Fatalf("%d units free while a job is inside its stop grace, want its 2 still held", got.Available[fakeCPUClassType])
	}

	releaseStop()
	eventually(t, "the stop has returned", idle(e, "stopping"))
	pass(t, ctx, e, pulling)
	if got := capacityOf(t, e); got.Available[fakeCPUClassType] != 5 || got.FreeCores != 5 {
		t.Fatalf("capacity = %+v, want 5 units and cores free once the job is gone", got)
	}
	releasePull()
	eventually(t, "the pulling job runs", running(f, "pulling"))
}

// A job names its type in whatever casing; the node's class is the same type in any of them.
func TestACoreJobIsRecognisedWhateverTheCasingOfItsType(t *testing.T) {
	f := newFakeEngine()
	e := classedExecutor(t, f, 8, "/dev/fake/0")
	job := coreJob("cores", 2)
	job.AcceleratorType = domain.AcceleratorType(strings.ToUpper(fakeCPUClassType))
	job.Job.AcceleratorType = job.AcceleratorType

	pass(t, t.Context(), e, job)
	eventually(t, "the job runs", running(f, "cores"))
	eventually(t, "its start has returned", idle(e, "cores"))

	if got := f.devices("cores"); len(got) != 0 {
		t.Errorf("the job was handed %v", got)
	}
	if got := capacityOf(t, e); got.Available[fakeCPUClassType] != 6 {
		t.Errorf("%d units free, want 6 of 8 with 2 held under another casing", got.Available[fakeCPUClassType])
	}
}

// A node offers the class it was configured with and no other. A job on another class, or on
// any class at a node with none, is started nowhere: run on cores nobody published, it would be
// charged for a type the node does not have.
func TestAJobOnAClassTheNodeDoesNotOfferIsNotStarted(t *testing.T) {
	other := coreJob("other", 2)
	other.AcceleratorType, other.Job.AcceleratorType = workloadkeys.CPUClassLabel+"=another", workloadkeys.CPUClassLabel+"=another"

	f := newFakeEngine()
	classed := classedExecutor(t, f, 8)
	pass(t, t.Context(), classed, other)
	if got := f.state("other"); got != "" {
		t.Errorf("a job on another class got a %s container", got)
	}

	f = newFakeEngine()
	plain := newFakeExecutor(f, "/dev/fake/0")
	pass(t, t.Context(), plain, coreJob("cores", 2))
	if got := f.state("cores"); got != "" {
		t.Errorf("a job on a CPU class got a %s container on a node that offers none", got)
	}
}

func TestACoreJobIsNeverBuiltWithADevice(t *testing.T) {
	e := classedExecutor(t, newFakeEngine(), 8, "/dev/fake/0")
	job := coreJob("cores", 1)

	if _, err := e.BuildContainerSpec(job, Placement{DevicePaths: []string{"/dev/fake/0"}}); err == nil || !strings.Contains(err.Error(), "CPU class") {
		t.Fatalf("err = %v, want a refusal to pass a device to a job on cores", err)
	}
	spec, err := e.BuildContainerSpec(job, Placement{})
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Devices) != 0 || spec.Labels[LabelAcceleratorType] != fakeCPUClassType || spec.Labels[LabelAcceleratorCnt] != "1" || spec.Labels[LabelCPUCores] != "1000" {
		t.Errorf("spec devices %v, labels %v; want no device and the class, one unit and one core recorded", spec.Devices, spec.Labels)
	}
	// A chip job still needs its device.
	if _, err := e.BuildContainerSpec(chipJob("chip", "1"), Placement{}); err == nil {
		t.Error("a chip job was built with no device")
	}
}

// The same chip workload on a node with and without a CPU class: what the node reports of its
// chips, and what each job is given, is the same.
func TestAChipWorkloadRunsTheSameOnANodeThatAlsoOffersACPUClass(t *testing.T) {
	type outcome struct {
		States         map[string]string
		Devices        map[string][]string
		ChipsFree      int64
		ChipsTotal     int64
		FreeCores      float64
		DoubleBookings int
	}
	run := func(t *testing.T, classed bool) outcome {
		f := newFakeEngine()
		e := newFakeExecutor(f, "/dev/fake/0", "/dev/fake/1")
		if classed {
			e = classedExecutor(t, f, 16, "/dev/fake/0", "/dev/fake/1")
		} else {
			previous := totalCPUCores
			totalCPUCores = func() float64 { return 16 }
			t.Cleanup(func() { totalCPUCores = previous })
		}
		ctx := t.Context()
		jobs := []*domain.Experiment{chipJob("a", "2"), chipJob("b", "500m"), chipJob("c", "1")}
		// One at a time, so which job holds which chip is the order they were asked for in.
		pass(t, ctx, e, jobs[:1]...)
		eventually(t, "the first chip job runs", func() bool { return len(e.operations()) == 0 && f.state("a") == "running" })
		pass(t, ctx, e, jobs[:2]...)
		eventually(t, "two chip jobs run", func() bool { return len(e.operations()) == 0 && f.state("b") == "running" })
		pass(t, ctx, e, jobs...)

		available, total, _, _, err := e.GetLiveAcceleratorCapacitySnapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		free, _, err := e.GetLiveCPUCapacity(ctx)
		if err != nil {
			t.Fatal(err)
		}
		out := outcome{map[string]string{}, map[string][]string{}, available[fakeAcceleratorType], total[fakeAcceleratorType], free, len(f.doubleBooked)}
		for _, id := range []string{"a", "b", "c"} {
			out.States[id], out.Devices[id] = f.state(id), f.devices(id)
		}
		return out
	}

	plain, classed := run(t, false), run(t, true)

	if !reflect.DeepEqual(plain, classed) {
		t.Fatalf("a chip workload ran differently beside a CPU class:\nwithout %+v\nwith    %+v", plain, classed)
	}
	if plain.States["c"] != "" || plain.ChipsFree != 0 || plain.ChipsTotal != 2 || plain.FreeCores != 13.5 {
		t.Fatalf("the workload exercised too little to compare: %+v", plain)
	}
}

func TestACPUClassNameIsALabelValue(t *testing.T) {
	for _, name := range []string{"", "epyc-9354", "box-b_cpu.v2"} {
		if err := ValidCPUClass(name); err != nil {
			t.Errorf("%q: %v", name, err)
		}
	}
	for _, name := range []string{"two words", workloadkeys.CPUClassLabel + "=x", "-x", "a/b"} {
		if err := ValidCPUClass(name); err == nil {
			t.Errorf("%q was accepted", name)
		}
	}
}
