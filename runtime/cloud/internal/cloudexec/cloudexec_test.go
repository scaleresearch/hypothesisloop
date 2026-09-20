package cloudexec

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/workload"
	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider/fake"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/exitline"
)

const (
	gib     = int64(1) << 30
	h100    = "nvidia.com/gpu.product=NVIDIA-H100"
	testAPI = "https://api.example.invalid"
)

var testShapes = []Shape{
	{Name: "h100", AcceleratorType: h100, Accelerators: 1, CPUMillis: 8000, MemoryBytes: 32 * gib, StorageBytes: 100 * gib, MaxInstances: 2},
	{Name: "cpu", CPUMillis: 4000, MemoryBytes: 16 * gib, StorageBytes: 50 * gib, MaxInstances: 1},
}

func newTestExecutor(t *testing.T) (*Executor, *fake.Provider) {
	t.Helper()
	p := fake.New()
	e, err := New(Config{ClusterID: "fake-account-1", APIURL: testAPI, JobImage: "registry.example.invalid/job:1", Shapes: testShapes, Provider: p})
	if err != nil {
		t.Fatal(err)
	}
	return e, p
}

type goldenAnswer struct {
	Experiment                 domain.Experiment `json:"experiment"`
	OrdinaryStopGraceSeconds   int64             `json:"ordinary_stop_grace_seconds"`
	CheckpointStopGraceSeconds int64             `json:"checkpoint_stop_grace_seconds"`
}

func readGolden(t *testing.T) goldenAnswer {
	t.Helper()
	raw, err := os.ReadFile("../../../shared/testdata/grace_answer.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden goldenAnswer
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	return golden
}

// job is the shared reconcile answer's experiment under another id and attempt, with the command
// a rented instance needs. accelerators 0 makes it a CPU job.
func job(t *testing.T, id string, attempt, accelerators int) *domain.Experiment {
	t.Helper()
	exp := readGolden(t).Experiment
	exp.ID, exp.AttemptCount = id, attempt
	resolved := *exp.ResolvedJob
	resolved.Command, resolved.Args = []string{"python", "train.py"}, []string{"--epochs", "3"}
	if accelerators > 0 {
		exp.AcceleratorType, exp.AcceleratorCount = h100, accelerators
		resolved.AcceleratorType, resolved.AcceleratorCount = h100, accelerators
	}
	exp.ResolvedJob = &resolved
	return &exp
}

func mustCreate(t *testing.T, e *Executor, exp *domain.Experiment) {
	t.Helper()
	if err := e.CreateWorkload(context.Background(), exp); err != nil {
		t.Fatal(err)
	}
}

func status(t *testing.T, e *Executor, id string) (workload.JobPhase, int) {
	t.Helper()
	observed, err := e.PollJobStatus(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return observed.Phase, observed.Attempt
}

func TestAnInstanceNameIsReadBackAsWhatItWasBuiltFrom(t *testing.T) {
	name := instanceName("exp-with-dashes-a2", 7, "h100", 1)
	if name != "hl-exp-with-dashes-a2-a7-h100-1" {
		t.Fatalf("name = %q", name)
	}
	parsed, ok := parseInstanceName(name)
	if !ok || parsed.experimentID != "exp-with-dashes-a2" || parsed.attempt != 7 || parsed.shape != "h100" || parsed.slot != 1 {
		t.Fatalf("parsed = %+v, %v", parsed, ok)
	}
	for _, foreign := range []string{"hl-agent-pe-bot", "hl-exp-a1-h100", "hl--a1-h100-0", "hl-exp-a01-h100-0", "other-exp-a1-h100-0"} {
		if _, ok := parseInstanceName(foreign); ok {
			t.Errorf("%q was read as a job's instance", foreign)
		}
	}
}

func TestAJobIsRentedAsOneInstanceRunningItsCommandUnderTheWrapper(t *testing.T) {
	e, p := newTestExecutor(t)
	mustCreate(t, e, job(t, "exp-1", 0, 1))

	spec := p.Spec("hl-exp-1-a0-h100-0")
	wantCommand := []string{"hl", "exec", "--stop-grace", "600", "--", "python", "train.py"}
	if !reflect.DeepEqual(spec.Command, wantCommand) || !reflect.DeepEqual(spec.Args, []string{"--epochs", "3"}) {
		t.Errorf("command = %q %q", spec.Command, spec.Args)
	}
	if spec.Image != "registry.example.invalid/job:1" || spec.AcceleratorType != h100 || spec.AcceleratorCount != 1 ||
		spec.CPUMillis != 1000 || spec.MemoryBytes != gib || spec.DiskBytes != gib {
		t.Errorf("spec = %+v", spec)
	}
	if spec.Env["HYPOTHESISLOOP_API_URL"] != testAPI || spec.Env["HYPOTHESISLOOP_EXPERIMENT_ID"] != "exp-1" || spec.Env["HYPOTHESISLOOP_ATTEMPT"] != "0" {
		t.Errorf("env = %v", spec.Env)
	}

	mustCreate(t, e, job(t, "exp-1", 0, 1))
	if names := p.Names(); len(names) != 1 {
		t.Fatalf("a second pass rented again: %v", names)
	}
}

func TestAJobThatCannotRunAsOneWrappedInstanceIsRefused(t *testing.T) {
	e, p := newTestExecutor(t)
	noCommand := job(t, "exp-1", 0, 0)
	noCommand.ResolvedJob.Command = nil
	twoNodes := job(t, "exp-2", 0, 0)
	twoNodes.ResolvedJob.NumNodes = 2
	hostMount := job(t, "exp-3", 0, 0)
	hostMount.ResolvedJob.HostMounts = map[string]string{"/data": "/mnt/data"}
	for _, exp := range []*domain.Experiment{noCommand, twoNodes, hostMount} {
		if err := e.CreateWorkload(context.Background(), exp); err == nil {
			t.Errorf("%s was rented", exp.ID)
		}
	}
	if names := p.Names(); len(names) != 0 || e.SupportsMultiNodeJobs() {
		t.Fatalf("rented %v, multi-node %v", names, e.SupportsMultiNodeJobs())
	}
}

func TestAJobWithoutAcceleratorsLeavesTheAcceleratorShapesFree(t *testing.T) {
	e, p := newTestExecutor(t)
	mustCreate(t, e, job(t, "exp-cpu", 0, 0))
	mustCreate(t, e, job(t, "exp-gpu", 0, 1))
	mustCreate(t, e, job(t, "exp-cpu-2", 0, 0))
	want := []string{"hl-exp-cpu-2-a0-h100-1", "hl-exp-cpu-a0-cpu-0", "hl-exp-gpu-a0-h100-0"}
	if names := p.Names(); !reflect.DeepEqual(names, want) {
		t.Fatalf("rented %v, want %v", names, want)
	}
}

func TestMaxInstancesBoundsWhatIsRented(t *testing.T) {
	e, p := newTestExecutor(t)
	mustCreate(t, e, job(t, "exp-1", 0, 1))
	mustCreate(t, e, job(t, "exp-2", 0, 1))
	if err := e.CreateWorkload(context.Background(), job(t, "exp-3", 0, 1)); err == nil {
		t.Fatal("a third instance of a shape capped at two was rented")
	}
	// An exited instance is still rented, so it still holds its slot.
	p.Exit("hl-exp-1-a0-h100-0", exitline.Format(0))
	if err := e.CreateWorkload(context.Background(), job(t, "exp-3", 0, 1)); err == nil {
		t.Fatal("a slot held by an exited instance was rented again")
	}
	if err := e.ReapTerminal(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	mustCreate(t, e, job(t, "exp-3", 0, 1))
	if names := p.Names(); !reflect.DeepEqual(names, []string{"hl-exp-2-a0-h100-1", "hl-exp-3-a0-h100-0"}) {
		t.Fatalf("rented %v", names)
	}
}

func TestCapacityIsTheShapesMinusWhatIsRentedRightNow(t *testing.T) {
	e, p := newTestExecutor(t)
	ctx := context.Background()

	read := func() (cpu float64, accelerators int64, nodes map[string]map[string]int64, byNode map[string]map[string]int64) {
		t.Helper()
		cpu, cpuTotal, err := e.GetLiveCPUCapacity(ctx)
		if err != nil || cpuTotal != 20 {
			t.Fatalf("cpu total = %v, %v", cpuTotal, err)
		}
		available, total, byNode, labels, err := e.GetLiveAcceleratorCapacitySnapshot(ctx)
		if err != nil || total[h100] != 2 || len(labels) != 3 || labels["h100-1"]["nvidia.com/gpu.product"] != "NVIDIA-H100" || len(labels["cpu-0"]) != 0 {
			t.Fatalf("accelerator snapshot = %v %v %v, %v", total, byNode, labels, err)
		}
		nodes, err = e.GetLiveNodeResourceCapacity(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return cpu, available[h100], nodes, byNode
	}

	cpu, accelerators, nodes, byNode := read()
	if cpu != 20 || accelerators != 2 || nodes["h100-0"][domain.NodeResourceMemoryBytes] != 32*gib || byNode["h100-0"][h100] != 1 {
		t.Fatalf("empty account: cpu %v, accelerators %v, nodes %v, by node %v", cpu, accelerators, nodes, byNode)
	}
	if _, isAcceleratorNode := byNode["cpu-0"]; isAcceleratorNode {
		t.Fatal("the cpu shape was reported as an accelerator node")
	}

	mustCreate(t, e, job(t, "exp-1", 0, 1))
	cpu, accelerators, nodes, byNode = read()
	if cpu != 12 || accelerators != 1 || nodes["h100-0"][domain.NodeResourceCPUMillicores] != 0 || byNode["h100-0"][h100] != 0 || byNode["h100-1"][h100] != 1 {
		t.Fatalf("one rented: cpu %v, accelerators %v, nodes %v, by node %v", cpu, accelerators, nodes, byNode)
	}

	// The account is the only record: an instance that goes away outside this process frees its
	// slot on the next read.
	if err := p.Destroy(ctx, "fake-1"); err != nil {
		t.Fatal(err)
	}
	if cpu, accelerators, _, _ = read(); cpu != 20 || accelerators != 2 {
		t.Fatalf("after the instance went away: cpu %v, accelerators %v", cpu, accelerators)
	}

	total, err := e.GetNodeTotalCapacity(ctx)
	if err != nil || len(total) != 3 || total["cpu-0"][domain.NodeResourceStorageBytes] != 50*gib {
		t.Fatalf("node totals = %v, %v", total, err)
	}
}

func TestARunningJobReportsTheSlotItOccupies(t *testing.T) {
	e, p := newTestExecutor(t)
	ctx := context.Background()
	mustCreate(t, e, job(t, "exp-1", 0, 1))

	_, _, _, scheduled, _, err := e.PollPhaseDetail(ctx, "exp-1", 0)
	if phase, _ := status(t, e, "exp-1"); phase != workload.JobPhasePending || scheduled != 0 || err != nil {
		t.Fatalf("starting instance: phase %v, scheduled %d, %v", phase, scheduled, err)
	}

	p.Start("hl-exp-1-a0-h100-0")
	_, _, _, scheduled, _, err = e.PollPhaseDetail(ctx, "exp-1", 0)
	if phase, _ := status(t, e, "exp-1"); phase != workload.JobPhaseRunning || scheduled != 1 || err != nil {
		t.Fatalf("running instance: phase %v, scheduled %d, %v", phase, scheduled, err)
	}
	acceleratorType, node, consistent, err := e.ResolveAdmittedAcceleratorType(ctx, "exp-1")
	if acceleratorType != h100 || node != "h100-0" || !consistent || err != nil {
		t.Fatalf("admitted = %q on %q, %v, %v", acceleratorType, node, consistent, err)
	}
}

func TestTheExitLineDecidesHowAnExitedInstanceEnded(t *testing.T) {
	for name, tc := range map[string]struct {
		lines       []string
		phase       workload.JobPhase
		reason      string
		messagePart string
	}{
		"zero":         {[]string{"done", exitline.Format(0)}, workload.JobPhaseSucceeded, "", ""},
		"non-zero":     {[]string{"Traceback", exitline.Format(3), "late stderr"}, workload.JobPhaseFailed, domain.PhaseReasonContainerFailed, "code 3"},
		"no exit line": {[]string{"epoch 4", "Killed"}, workload.JobPhaseFailed, "", "without an exit line"},
	} {
		t.Run(name, func(t *testing.T) {
			e, p := newTestExecutor(t)
			mustCreate(t, e, job(t, "exp-1", 0, 0))
			p.Start("hl-exp-1-a0-cpu-0")
			p.Exit("hl-exp-1-a0-cpu-0", tc.lines...)

			phase, _ := status(t, e, "exp-1")
			reason, message, _, _, _, err := e.PollPhaseDetail(context.Background(), "exp-1", 0)
			if err != nil || phase != tc.phase || reason != tc.reason || !strings.Contains(message, tc.messagePart) {
				t.Fatalf("phase %v, reason %q, message %q, %v", phase, reason, message, err)
			}
		})
	}
}

func TestAGrantedTerminationStopsARunningInstanceAndLeavesItToExit(t *testing.T) {
	e, p := newTestExecutor(t)
	ctx := context.Background()
	const name = "hl-exp-1-a0-cpu-0"
	mustCreate(t, e, job(t, "exp-1", 0, 0))
	p.Start(name)

	for pass := 1; pass <= 2; pass++ {
		if err := e.DeleteWorkload(ctx, "exp-1", true); err != nil {
			t.Fatal(err)
		}
		if p.Stops(name) != pass {
			t.Fatalf("pass %d: %d stops", pass, p.Stops(name))
		}
	}
	// Still rented and still actual, so its slot stays taken while it saves.
	if ids, _ := e.ListManagedJobs(ctx); !reflect.DeepEqual(ids, []string{"exp-1"}) {
		t.Fatalf("actual = %v", ids)
	}

	p.Exit(name, "checkpoint saved", exitline.Format(0))
	if ids, _ := e.ListManagedJobs(ctx); len(ids) != 0 {
		t.Fatalf("an exited instance is still actual: %v", ids)
	}
	if err := e.ReapTerminal(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if names := p.Names(); len(names) != 0 {
		t.Fatalf("still rented after it exited: %v", names)
	}
}

func TestATerminationWithoutAGrantDestroysTheInstanceAtOnce(t *testing.T) {
	e, p := newTestExecutor(t)
	ctx := context.Background()
	mustCreate(t, e, job(t, "exp-running", 0, 0))
	p.Start("hl-exp-running-a0-cpu-0")
	mustCreate(t, e, job(t, "exp-starting", 0, 1))

	// A grant that has expired control-plane-side arrives as no grant, which is what ends an
	// instance whose wrapper never does.
	if err := e.DeleteWorkload(ctx, "exp-running", false); err != nil {
		t.Fatal(err)
	}
	// A granted window has nothing to save in an instance whose command never ran.
	if err := e.DeleteWorkload(ctx, "exp-starting", true); err != nil {
		t.Fatal(err)
	}
	if names := p.Names(); len(names) != 0 {
		t.Fatalf("still rented: %v", names)
	}
	if phase, attempt := status(t, e, "exp-running"); phase != workload.JobPhaseGone || attempt != workload.AttemptUnknown {
		t.Fatalf("destroyed instance reads as %v attempt %d", phase, attempt)
	}
}

func TestAnInstanceThatIsNotAJobsIsLeftAlone(t *testing.T) {
	e, p := newTestExecutor(t)
	ctx := context.Background()
	if _, err := p.Create(ctx, provider.Spec{Name: "hl-agent-pe-bot"}); err != nil {
		t.Fatal(err)
	}
	mustCreate(t, e, job(t, "exp-1", 0, 0))
	ids, err := e.ListManagedJobsForStatus(ctx)
	if err != nil || !reflect.DeepEqual(ids, []string{"exp-1"}) {
		t.Fatalf("managed = %v, %v", ids, err)
	}
	if err := e.ReapTerminal(ctx, nil); err != nil || len(p.Names()) != 2 {
		t.Fatalf("reap: %v, rented %v", err, p.Names())
	}
}

func TestShapesAreValidatedAtStartup(t *testing.T) {
	valid := testShapes[0]
	for name, mutate := range map[string]func(*Shape){
		"dash in name":       func(s *Shape) { s.Name = "h100-x1" },
		"no max instances":   func(s *Shape) { s.MaxInstances = 0 },
		"type without count": func(s *Shape) { s.Accelerators = 0 },
		"count without type": func(s *Shape) { s.AcceleratorType = "" },
		"type not a label":   func(s *Shape) { s.AcceleratorType = "H100" },
		"no memory":          func(s *Shape) { s.MemoryBytes = 0 },
	} {
		shape := valid
		mutate(&shape)
		if _, err := New(Config{ClusterID: "c", APIURL: testAPI, JobImage: "i", Shapes: []Shape{shape}, Provider: fake.New()}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := New(Config{ClusterID: "c", APIURL: testAPI, JobImage: "i", Shapes: []Shape{valid, valid}, Provider: fake.New()}); err == nil {
		t.Error("two shapes of one name were accepted")
	}
}
