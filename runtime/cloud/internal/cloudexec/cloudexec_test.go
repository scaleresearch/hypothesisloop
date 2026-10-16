package cloudexec

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/runtime/shared/agentexec"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/contract/exitline"
	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider/fake"
)

const (
	gib       = int64(1) << 30
	h100      = "nvidia.com/gpu.product=NVIDIA-H100"
	testAPI   = "https://api.example.invalid"
	testImage = "registry.example.invalid/job:1"
	// testEnvironments is the repository the environments built on testImage are in.
	testEnvironments = "registry.test/hl-environments/environment"
)

var testShapes = []Shape{
	{Name: "h100", AcceleratorType: h100, Accelerators: 1, CPUMillis: 8000, MemoryBytes: 32 * gib, StorageBytes: 100 * gib, MaxInstances: 2},
	{Name: "cpu", CPUMillis: 4000, MemoryBytes: 16 * gib, StorageBytes: 50 * gib, MaxInstances: 1},
}

func newTestExecutor(t *testing.T) (*Executor, *fake.Provider) {
	t.Helper()
	p := fake.New()
	e, err := New(testConfig(p))
	if err != nil {
		t.Fatal(err)
	}
	return e, p
}

func testConfig(p provider.Provider) Config {
	return Config{ClusterID: "fake-account-1", Namespace: "t1", APIURL: testAPI, JobImage: testImage, EnvironmentsRepository: testEnvironments, OfferWait: 3 * time.Minute, Startup: 4 * time.Minute, Shapes: testShapes, Provider: p}
}

type goldenAnswer struct {
	Experiment                 clusterapi.DesiredJob `json:"experiment"`
	OrdinaryStopGraceSeconds   int64                 `json:"ordinary_stop_grace_seconds"`
	CheckpointStopGraceSeconds int64                 `json:"checkpoint_stop_grace_seconds"`
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
// a rented instance needs, admitted to the first slot of the shape it asks for: h100-0 with
// accelerators, cpu-0 without.
func job(t *testing.T, id string, attempt, accelerators int) *clusterapi.DesiredJob {
	t.Helper()
	exp := readGolden(t).Experiment
	exp.ID, exp.AttemptCount = id, attempt
	resolved := *exp.ResolvedJob
	resolved.Command, resolved.Args = []string{"python", "train.py"}, []string{"--epochs", "3"}
	resolved.Image = testImage
	resolved.NodeSelector = map[string]string{clusterapi.NodeSlotLabel: "cpu-0"}
	if accelerators > 0 {
		resolved.NodeSelector = map[string]string{clusterapi.NodeSlotLabel: "h100-0"}
		exp.AcceleratorType, exp.AcceleratorCount = h100, accelerators
		resolved.AcceleratorType, resolved.AcceleratorCount = h100, accelerators
	}
	exp.ResolvedJob = &resolved
	return &exp
}

// on moves exp to another slot, as admission would have placed it.
func on(exp *clusterapi.DesiredJob, slot string) *clusterapi.DesiredJob {
	exp.ResolvedJob.NodeSelector = map[string]string{clusterapi.NodeSlotLabel: slot}
	return exp
}

// asHanded is exp as a reconcile answer carries it to a runtime.
func asHanded(t *testing.T, exp *clusterapi.DesiredJob) *clusterapi.DesiredJob {
	t.Helper()
	wire, err := json.Marshal(exp)
	if err != nil {
		t.Fatal(err)
	}
	var handed clusterapi.DesiredJob
	if err := json.Unmarshal(wire, &handed); err != nil {
		t.Fatal(err)
	}
	return &handed
}

func mustCreate(t *testing.T, e *Executor, exp *clusterapi.DesiredJob) {
	t.Helper()
	if err := e.CreateWorkload(context.Background(), exp, nil); err != nil {
		t.Fatal(err)
	}
}

func status(t *testing.T, e *Executor, id string) (clusterapi.JobPhase, int) {
	t.Helper()
	observed, err := e.PollJobStatus(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return observed.Phase, observed.Attempt
}

func TestAnInstanceNameIsReadBackAsWhatItWasBuiltFrom(t *testing.T) {
	e, _ := newTestExecutor(t)
	name := e.instanceName("exp-with-dashes-a2", 7, "h100", 1)
	if name != "hl-job-t1-exp-with-dashes-a2-a7-h100-1" {
		t.Fatalf("name = %q", name)
	}
	parsed, ok := e.parseInstanceName(name)
	if !ok || parsed.experimentID != "exp-with-dashes-a2" || parsed.attempt != 7 || parsed.slot() != "h100-1" {
		t.Fatalf("parsed = %+v, %v", parsed, ok)
	}
	const issued = "job-0199c2a4b7d07f3a9c1e5b2d4f608a13"
	name = e.instanceName(issued, attemptBudget, "h100", 1)
	parsed, ok = e.parseInstanceName(name)
	if !ok || parsed.experimentID != issued || parsed.attempt != attemptBudget || parsed.slot() != "h100-1" {
		t.Fatalf("issued id: parsed = %+v, %v from %q", parsed, ok, name)
	}
	if err := provider.ValidateName(name); err != nil {
		t.Fatalf("issued id: %v", err)
	}
	for _, foreign := range []string{
		"hl-agent-t1-pe-bot", "hl-job-t1-exp-a1-h100", "hl-job-t1--a1-h100-0", "hl-job-t1-exp-a01-h100-0",
		"hl-job-t2-exp-a1-h100-0", "hl-job-t10-exp-a1-h100-0", "hl-exp-a1-h100-0",
	} {
		if _, ok := e.parseInstanceName(foreign); ok {
			t.Errorf("%q was read as a job's instance of namespace t1", foreign)
		}
	}
}

func TestAJobIsRentedAsOneInstanceRunningItsCommandUnderTheWrapper(t *testing.T) {
	e, p := newTestExecutor(t)
	mustCreate(t, e, job(t, "exp-1", 0, 1))

	spec := p.Spec("hl-job-t1-exp-1-a0-h100-0")
	wantCommand := []string{"hl", "exec", "--stop-grace", "600", "--", "python", "train.py", "--epochs", "3"}
	if !reflect.DeepEqual(spec.Command, wantCommand) {
		t.Errorf("command = %q", spec.Command)
	}
	if spec.Image != testImage || spec.AcceleratorType != h100 || spec.AcceleratorCount != 1 ||
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
	for _, exp := range []*clusterapi.DesiredJob{noCommand, twoNodes, hostMount} {
		if err := e.CreateWorkload(context.Background(), exp, nil); err == nil {
			t.Errorf("%s was rented", exp.ID)
		}
	}
	if names := p.Names(); len(names) != 0 || e.SupportsMultiNodeJobs() {
		t.Fatalf("rented %v, multi-node %v", names, e.SupportsMultiNodeJobs())
	}
}

// An instance boots the image desired state names: the job image, or an environment of the
// environments repository by digest, which it pulls with the reconcile answer's login. Any other
// image, or none, is one it does not rent for.
func TestAnInstanceBootsTheJobImageOrAnEnvironmentAndNothingElse(t *testing.T) {
	e, p := newTestExecutor(t)
	pull := &clusterapi.RegistryLogin{Registry: "registry.test", Username: "oauth2accesstoken", Password: "pull-token"}
	if err := e.CreateWorkload(context.Background(), job(t, "exp-1", 0, 0), pull); err != nil {
		t.Fatal(err)
	}
	environment := on(job(t, "exp-2", 0, 0), "h100-1")
	environment.ResolvedJob.Image = testEnvironments + "@sha256:" + strings.Repeat("ab", 32)
	if err := e.CreateWorkload(context.Background(), environment, pull); err != nil {
		t.Fatal(err)
	}
	names := p.Names()
	if len(names) != 2 {
		t.Fatalf("rented %v, want both jobs", names)
	}
	for _, name := range names {
		spec := p.Spec(name)
		want, wantLogin := testImage, (*provider.ImageLogin)(nil)
		if strings.Contains(name, "-exp-2-") {
			want, wantLogin = environment.ResolvedJob.Image, &provider.ImageLogin{Registry: "registry.test", Username: "oauth2accesstoken", Password: "pull-token"}
		}
		if spec.Image != want || !reflect.DeepEqual(spec.ImageLogin, wantLogin) {
			t.Errorf("%s boots %q with login %+v, want %q with %+v", name, spec.Image, spec.ImageLogin, want, wantLogin)
		}
	}

	if err := e.CreateWorkload(context.Background(), on(environment, "cpu-0"), nil); err == nil || !strings.Contains(err.Error(), "registry_pull") {
		t.Fatalf("an environment with no login: err = %v, want one naming registry_pull", err)
	}
	for image, why := range map[string]string{
		"":                                  "no image",
		"registry.test/someone-else:latest": "another image",
		testEnvironments + ":latest":        "an environment by tag",
		testEnvironments + "@sha256:abc":    "a malformed digest",
		testEnvironments + "-other@sha256:" + strings.Repeat("ab", 32): "another repository",
	} {
		other := on(job(t, "exp-3", 0, 0), "cpu-0")
		other.ResolvedJob.Image = image
		var unrunnable *agentexec.Unrunnable
		if err := e.CreateWorkload(context.Background(), other, pull); !errors.As(err, &unrunnable) {
			t.Errorf("%s (%q): err = %v, want it unrunnable", why, image, err)
		}
	}
	if len(p.Names()) != 2 {
		t.Fatalf("rented %v, want nothing beyond the two jobs", p.Names())
	}
}

// Admission chose the slot. The runtime rents that one or reports why it cannot, and never
// picks another, even with one free.
func TestAJobIsRentedInExactlyTheSlotItWasAdmittedTo(t *testing.T) {
	e, p := newTestExecutor(t)
	ctx := context.Background()
	mustCreate(t, e, on(job(t, "exp-cpu", 0, 0), "h100-1"))
	mustCreate(t, e, job(t, "exp-gpu", 0, 1))
	if names := p.Names(); !reflect.DeepEqual(names, []string{"hl-job-t1-exp-cpu-a0-h100-1", "hl-job-t1-exp-gpu-a0-h100-0"}) {
		t.Fatalf("rented %v", names)
	}

	unpinned := job(t, "exp-unpinned", 0, 0)
	unpinned.ResolvedJob.NodeSelector = nil
	tooLarge := job(t, "exp-large", 0, 0)
	tooLarge.ResolvedJob.CPU = "6"
	otherType := on(job(t, "exp-type", 0, 1), "cpu-0")
	for want, exp := range map[string]*clusterapi.DesiredJob{
		"without a slot":         unpinned,
		"not configured":         on(job(t, "exp-nowhere", 0, 0), "h100-2"),
		"still holds":            on(job(t, "exp-second", 0, 0), "h100-0"),
		"exceed the slot":        tooLarge,
		"is not the slot's \"\"": otherType,
	} {
		if err := e.CreateWorkload(ctx, exp, nil); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want one saying %q", exp.ID, err, want)
		}
	}
	if names := p.Names(); len(names) != 2 {
		t.Fatalf("a refused job was rented: %v", names)
	}
}

// Admission folds the case of an accelerator type, so a job that wrote its type in another
// casing than the shape is admitted to the slot. Refused here it went round admit, refuse and
// evict for as long as it had requeues left.
func TestAJobIsNotRefusedForHowItCasedItsAcceleratorType(t *testing.T) {
	e, p := newTestExecutor(t)
	lowercase := job(t, "exp-1", 0, 1)
	lowercase.AcceleratorType = "nvidia.com/gpu.product=nvidia-h100"
	mustCreate(t, e, lowercase)
	p.Start("hl-job-t1-exp-1-a0-h100-0")
	// What is reported is the shape's type, which the control plane reads as the one asked for.
	admitted, _, _, err := e.ResolveAdmittedAcceleratorType(context.Background(), "exp-1")
	if err != nil || !admitted.Is(lowercase.AcceleratorType) {
		t.Fatalf("admitted %q, %v", admitted, err)
	}

	another := on(job(t, "exp-2", 0, 1), "h100-1")
	another.AcceleratorType = "nvidia.com/gpu.product=NVIDIA-L40"
	if err := e.CreateWorkload(context.Background(), another, nil); err == nil || !strings.Contains(err.Error(), "is not the slot's") {
		t.Fatalf("another type: err = %v", err)
	}
}

func TestAnInstanceNameThatTheProviderCannotHoldIsRefused(t *testing.T) {
	e, p := newTestExecutor(t)
	for _, id := range []string{strings.Repeat("x", 120), "Exp_1"} {
		if err := e.CreateWorkload(context.Background(), job(t, id, 0, 0), nil); err == nil || len(p.Names()) != 0 {
			t.Errorf("%q: err = %v, rented %v", id, err, p.Names())
		}
	}
	mustCreate(t, e, job(t, strings.Repeat("x", experimentIDBudget), attemptBudget, 0))
}

func TestASlotIsHeldUntilItsInstanceIsDestroyed(t *testing.T) {
	e, p := newTestExecutor(t)
	mustCreate(t, e, job(t, "exp-1", 0, 1))
	mustCreate(t, e, on(job(t, "exp-2", 0, 1), "h100-1"))
	// An exited instance is still rented, so it still holds its slot.
	p.Exit("hl-job-t1-exp-1-a0-h100-0", exitline.Format(0))
	if err := e.CreateWorkload(context.Background(), job(t, "exp-3", 0, 1), nil); err == nil {
		t.Fatal("a slot held by an exited instance was rented again")
	}
	if err := e.ReapTerminal(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	mustCreate(t, e, job(t, "exp-3", 0, 1))
	if names := p.Names(); !reflect.DeepEqual(names, []string{"hl-job-t1-exp-2-a0-h100-1", "hl-job-t1-exp-3-a0-h100-0"}) {
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
		if err != nil || total[h100] != 2 || len(labels) != 3 || labels["h100-1"]["nvidia.com/gpu.product"] != "NVIDIA-H100" ||
			labels["h100-1"][clusterapi.NodeSlotLabel] != "h100-1" || labels["h100-1"][clusterapi.NodeSlotAcceleratorsLabel] != "1" || !reflect.DeepEqual(labels["cpu-0"], map[string]string{clusterapi.NodeSlotLabel: "cpu-0"}) {
			t.Fatalf("accelerator snapshot = %v %v %v, %v", total, byNode, labels, err)
		}
		nodes, err = e.GetLiveNodeResourceCapacity(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return cpu, available[h100], nodes, byNode
	}

	cpu, accelerators, nodes, byNode := read()
	if cpu != 20 || accelerators != 2 || nodes["h100-0"][clusterapi.NodeResourceMemoryBytes] != 32*gib || byNode["h100-0"][h100] != 1 {
		t.Fatalf("empty account: cpu %v, accelerators %v, nodes %v, by node %v", cpu, accelerators, nodes, byNode)
	}
	if !reflect.DeepEqual(byNode["cpu-0"], map[string]int64{string(clusterapi.CPUType): 4}) || byNode["h100-0"][string(clusterapi.CPUType)] != 0 {
		t.Fatalf("by node %v: want the cpu shape's 4 cores as units of the cpu type, and no cores on a slot with a device", byNode)
	}

	// A slot is one instance however small the job: a 1-cpu job leaves none of its 8.
	mustCreate(t, e, job(t, "exp-1", 0, 1))
	cpu, accelerators, nodes, byNode = read()
	if cpu != 12 || accelerators != 1 || nodes["h100-0"][clusterapi.NodeResourceCPUMillicores] != 0 || byNode["h100-0"][h100] != 0 || byNode["h100-1"][h100] != 1 {
		t.Fatalf("one rented: cpu %v, accelerators %v, nodes %v, by node %v", cpu, accelerators, nodes, byNode)
	}

	// The account is the only record: an instance that goes away outside this process frees its
	// slot on the next read.
	if err := p.Destroy(ctx, "fake-0001"); err != nil {
		t.Fatal(err)
	}
	if cpu, accelerators, _, _ = read(); cpu != 20 || accelerators != 2 {
		t.Fatalf("after the instance went away: cpu %v, accelerators %v", cpu, accelerators)
	}

	total, err := e.GetNodeTotalCapacity(ctx)
	if err != nil || len(total) != 3 || total["cpu-0"][clusterapi.NodeResourceStorageBytes] != 50*gib {
		t.Fatalf("node totals = %v, %v", total, err)
	}
}

func TestARunningJobReportsTheSlotItOccupies(t *testing.T) {
	e, p := newTestExecutor(t)
	ctx := context.Background()
	mustCreate(t, e, job(t, "exp-1", 0, 1))

	_, _, _, scheduled, _, err := e.PollPhaseDetail(ctx, "exp-1", 0)
	if phase, _ := status(t, e, "exp-1"); phase != clusterapi.JobPhasePending || scheduled != 0 || err != nil {
		t.Fatalf("starting instance: phase %v, scheduled %d, %v", phase, scheduled, err)
	}

	p.Start("hl-job-t1-exp-1-a0-h100-0")
	_, _, _, scheduled, _, err = e.PollPhaseDetail(ctx, "exp-1", 0)
	if phase, _ := status(t, e, "exp-1"); phase != clusterapi.JobPhaseRunning || scheduled != 1 || err != nil {
		t.Fatalf("running instance: phase %v, scheduled %d, %v", phase, scheduled, err)
	}
	acceleratorType, node, consistent, err := e.ResolveAdmittedAcceleratorType(ctx, "exp-1")
	if acceleratorType != h100 || node != "h100-0" || !consistent || err != nil {
		t.Fatalf("admitted = %q on %q, %v, %v", acceleratorType, node, consistent, err)
	}
}

// Two jobs of one spec land on different rented machines, so a job says which one it has: the
// instance always, and of the machine behind it what the provider tells.
func TestAJobReportsTheMachineItsInstanceIsOn(t *testing.T) {
	e, p := newTestExecutor(t)
	ctx := context.Background()
	mustCreate(t, e, job(t, "exp-1", 0, 1))

	observed, err := e.PollJobStatus(ctx, "exp-1")
	if want := (clusterapi.JobMachine{InstanceID: "fake-0001"}); err != nil || observed.Machine == nil || *observed.Machine != want {
		t.Fatalf("a provider that says nothing of the machine: %+v, %v, want %+v", observed.Machine, err, want)
	}
	p.RunsOn("hl-job-t1-exp-1-a0-h100-0", "machine-11230", "H100", "AMD EPYC 7B13", 2.25)
	observed, err = e.PollJobStatus(ctx, "exp-1")
	want := clusterapi.JobMachine{InstanceID: "fake-0001", HostID: "machine-11230", AcceleratorModel: "H100", CPUModel: "AMD EPYC 7B13", PricePerHourUSD: 2.25}
	if err != nil || observed.Machine == nil || *observed.Machine != want {
		t.Fatalf("machine = %+v, %v, want %+v", observed.Machine, err, want)
	}
}

func TestTheExitLineDecidesHowAnExitedInstanceEnded(t *testing.T) {
	for name, tc := range map[string]struct {
		lines       []string
		phase       clusterapi.JobPhase
		reason      string
		messagePart string
	}{
		"zero":     {[]string{"done", exitline.Format(0)}, clusterapi.JobPhaseSucceeded, "", ""},
		"non-zero": {[]string{"Traceback", exitline.Format(3), "late stderr"}, clusterapi.JobPhaseFailed, clusterapi.PhaseReasonContainerFailed, "code 3"},
		// The wrapper died with its machine: an infrastructure outcome, never the job's failure.
		"no exit line": {[]string{"epoch 4", "Killed"}, clusterapi.JobPhaseGone, "", ""},
		"no output":    {nil, clusterapi.JobPhaseGone, "", ""},
		// A job that prints the line itself and goes on is judged by the wrapper's, the last.
		"last line wins": {[]string{exitline.Format(0), "more", exitline.Format(7)}, clusterapi.JobPhaseFailed, clusterapi.PhaseReasonContainerFailed, "code 7"},
	} {
		t.Run(name, func(t *testing.T) {
			e, p := newTestExecutor(t)
			mustCreate(t, e, job(t, "exp-1", 0, 0))
			p.Start("hl-job-t1-exp-1-a0-cpu-0")
			p.Exit("hl-job-t1-exp-1-a0-cpu-0", tc.lines...)

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
	const name = "hl-job-t1-exp-1-a0-cpu-0"
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
	p.Start("hl-job-t1-exp-running-a0-cpu-0")
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
	if phase, attempt := status(t, e, "exp-running"); phase != clusterapi.JobPhaseGone || attempt != clusterapi.AttemptUnknown {
		t.Fatalf("destroyed instance reads as %v attempt %d", phase, attempt)
	}
}

// The account is shared: with other agents, each under its own namespace, with the instances
// agents themselves run in, and with whatever else is rented on it. None of that is this
// executor's to list, count against a slot, stop or destroy.
func TestInstancesOutsideTheNamespaceAreNeverListedCountedOrDestroyed(t *testing.T) {
	e, p := newTestExecutor(t)
	ctx := context.Background()
	foreign := []string{
		"hl-agent-t1-pe-bot",         // the launcher's, same namespace
		"hl-job-t2-exp-1-a0-cpu-0",   // another namespace's job of the same id in the same slot
		"hl-job-t10-exp-1-a0-h100-0", // a namespace this one is a prefix of
		"hl-exp-1-a0-h100-1",         // not the platform's naming at all
	}
	for _, name := range foreign {
		if _, err := p.Create(ctx, provider.Spec{Name: name}); err != nil {
			t.Fatal(err)
		}
		p.Start(name)
	}

	if ids, err := e.ListManagedJobsForStatus(ctx); err != nil || len(ids) != 0 {
		t.Fatalf("managed = %v, %v", ids, err)
	}
	cpu, cpuTotal, err := e.GetLiveCPUCapacity(ctx)
	available, total, _, _, acceleratorErr := e.GetLiveAcceleratorCapacitySnapshot(ctx)
	if err != nil || acceleratorErr != nil || cpu != cpuTotal || available[h100] != total[h100] {
		t.Fatalf("capacity %v/%v, %v/%v: a foreign instance took a slot", cpu, cpuTotal, available, total)
	}
	if phase, _ := status(t, e, "exp-1"); phase != clusterapi.JobPhaseGone {
		t.Fatalf("another namespace's exp-1 was reported as %v", phase)
	}

	exp := job(t, "exp-1", 0, 0)
	mustCreate(t, e, exp)
	if matches, err := e.WorkloadMatchesDesired(ctx, exp); err != nil || !matches {
		t.Fatalf("matches = %v, %v", matches, err)
	}
	if err := e.DeleteWorkload(ctx, "exp-1", true); err != nil {
		t.Fatal(err)
	}
	if err := e.ReapTerminal(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if names := p.Names(); !reflect.DeepEqual(names, append([]string(nil), sorted(foreign)...)) {
		t.Fatalf("rented %v, want every foreign instance and nothing else", names)
	}
	for _, name := range foreign {
		if p.Stops(name) != 0 {
			t.Errorf("%s was stopped", name)
		}
	}
}

func sorted(names []string) []string {
	out := append([]string(nil), names...)
	sort.Strings(out)
	return out
}

func TestTheConfigurationIsValidatedAtStartup(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"dash in shape name":     func(c *Config) { c.Shapes[0].Name = "h100-x1" },
		"uppercase shape name":   func(c *Config) { c.Shapes[0].Name = "H100" },
		"shape name too long":    func(c *Config) { c.Shapes[0].Name = "a1234567890123456" },
		"no max instances":       func(c *Config) { c.Shapes[0].MaxInstances = 0 },
		"type without count":     func(c *Config) { c.Shapes[0].Accelerators = 0 },
		"count without type":     func(c *Config) { c.Shapes[0].AcceleratorType = "" },
		"type not a label":       func(c *Config) { c.Shapes[0].AcceleratorType = "H100" },
		"type in another casing": func(c *Config) { c.Shapes[0].AcceleratorType = "nvidia.com/gpu.product=nvidia-h100" },
		"no memory":              func(c *Config) { c.Shapes[0].MemoryBytes = 0 },
		"two shapes of one name": func(c *Config) { c.Shapes = append(c.Shapes, c.Shapes[0]) },
		"no namespace":           func(c *Config) { c.Namespace = "" },
		"dash in namespace":      func(c *Config) { c.Namespace = "team-a" },
		"namespace too long":     func(c *Config) { c.Namespace = "a1234567890123456" },
		"no job image":           func(c *Config) { c.JobImage = "" },
		"environments by digest": func(c *Config) { c.EnvironmentsRepository = testEnvironments + "@sha256:abc" },
		"environments no path":   func(c *Config) { c.EnvironmentsRepository = "registry.test" },
		"name beyond the provider": func(c *Config) {
			c.Namespace, c.Shapes[0].Name, c.Shapes[0].MaxInstances = "a123456789012345", "b123456789012345", 1<<62
		},
	} {
		cfg := testConfig(fake.New())
		cfg.Shapes = []Shape{testShapes[0]}
		mutate(&cfg)
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// What a rented instance can never run is said as such, in words for the job's owner, so that
// the loop hands the job to the control plane instead of asking for it again on every pass. A
// vendor that refuses a create is not that: the same job may be rented on the next pass.
func TestAJobARentedInstanceCanNeverRunIsUnrunnableAndAFailedRentIsNot(t *testing.T) {
	e, p := newTestExecutor(t)
	if e.OnlyImage() != testImage {
		t.Fatalf("OnlyImage() = %q, want the image every instance boots", e.OnlyImage())
	}
	for name, c := range map[string]struct {
		spoil func(*clusterapi.JobSpec)
		says  string
	}{
		"host mounts": {func(s *clusterapi.JobSpec) { s.HostMounts = map[string]string{"/data": "/data"} }, "host_mounts"},
		"no command":  {func(s *clusterapi.JobSpec) { s.Command, s.Args = nil, nil }, "no command"},
		"two nodes":   {func(s *clusterapi.JobSpec) { s.NumNodes = 2 }, "2 nodes"},
	} {
		exp := job(t, "exp-"+strings.ReplaceAll(name, " ", "-"), 0, 0)
		c.spoil(exp.ResolvedJob)
		err := e.CreateWorkload(context.Background(), exp, nil)
		var unrunnable *agentexec.Unrunnable
		if !errors.As(err, &unrunnable) || !strings.Contains(unrunnable.Why, c.says) {
			t.Errorf("%s: err = %v, want an Unrunnable that says %q", name, err, c.says)
		}
	}
	if p.Calls("Create") != 0 {
		t.Fatalf("rented %d instances for jobs that cannot run", p.Calls("Create"))
	}

	p.Fail = func(method, _ string) error {
		if method == "Create" {
			return errors.New("vendor: HTTP 503")
		}
		return nil
	}
	err := e.CreateWorkload(context.Background(), job(t, "exp-later", 0, 1), nil)
	var unrunnable *agentexec.Unrunnable
	if err == nil || errors.As(err, &unrunnable) {
		t.Fatalf("a rent the vendor refused: err = %v, want an error that is not Unrunnable", err)
	}
}

// A destroy the vendor refused is asked for again by the next pass and by nothing else: leaving
// destroys nothing and names what stays rented.
func TestAFailedDestroyIsAskedForAgainByTheNextPassAndLeavingDestroysNothing(t *testing.T) {
	e, p := newTestExecutor(t)
	ctx := context.Background()
	done, running := job(t, "exp-done", 0, 1), on(job(t, "exp-running", 0, 1), "h100-1")
	mustCreate(t, e, done)
	mustCreate(t, e, running)
	p.Start(e.instanceName("exp-done", 0, "h100", 0))
	p.Start(e.instanceName("exp-running", 0, "h100", 1))
	p.Exit(e.instanceName("exp-done", 0, "h100", 0), "done")
	desired := map[string]*clusterapi.DesiredJob{"exp-running": running}

	refused := errors.New("vendor: HTTP 429")
	p.Fail = func(method, _ string) error {
		if method == "Destroy" {
			return refused
		}
		return nil
	}
	if err := e.ReapTerminal(ctx, desired); !errors.Is(err, refused) {
		t.Fatalf("ReapTerminal = %v, want the refused destroy", err)
	}
	destroys := p.Calls("Destroy")
	if err := e.Leave(ctx); err != nil || p.Calls("Destroy") != destroys {
		t.Fatalf("Leave: err = %v, destroys %d then %d, want none asked for", err, destroys, p.Calls("Destroy"))
	}

	p.Fail = nil
	if err := e.ReapTerminal(ctx, desired); err != nil {
		t.Fatalf("the next pass: %v", err)
	}
	if names := p.Names(); len(names) != 1 || names[0] != e.instanceName("exp-running", 0, "h100", 1) {
		t.Fatalf("rented after the next pass: %v, want the running job alone", names)
	}
}

// A shape without accelerators offers its whole cores as units of the CPU type, so a job on CPU
// can be placed on it, and a job on CPU is rented as an instance with no device.
func TestACPUShapeOffersItsCoresAndAJobOnCPURentsNoDevice(t *testing.T) {
	e, p := newTestExecutor(t)
	ctx := context.Background()
	cores := string(clusterapi.CPUType)
	available, total, _, _, err := e.GetLiveAcceleratorCapacitySnapshot(ctx)
	if err != nil || available[cores] != 4 || total[cores] != 4 {
		t.Fatalf("cores %d of %d, %v, want the cpu shape's 4", available[cores], total[cores], err)
	}

	exp := job(t, "exp-cores", 0, 0)
	exp.AcceleratorType, exp.AcceleratorCount = clusterapi.CPUType, 4
	exp.ResolvedJob.AcceleratorType, exp.ResolvedJob.AcceleratorCount, exp.ResolvedJob.CPU = clusterapi.CPUType, 4, "4"
	mustCreate(t, e, exp)
	instances, err := p.List(ctx, "")
	if err != nil || len(instances) != 1 {
		t.Fatalf("instances %v, %v", instances, err)
	}
	spec := p.Spec(instances[0].Name)
	if spec.AcceleratorType != "" || spec.AcceleratorCount != 0 || spec.CPUMillis != 4000 {
		t.Fatalf("rented %+v, want 4 cores and no device", spec)
	}
	available, _, _, _, err = e.GetLiveAcceleratorCapacitySnapshot(ctx)
	if err != nil || available[cores] != 0 {
		t.Fatalf("cores available %d once the slot is rented, %v", available[cores], err)
	}
}
