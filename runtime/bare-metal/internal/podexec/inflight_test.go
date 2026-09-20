package podexec

import (
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/workload"
)

const fakeAcceleratorType = "example.com/chip=fake"

type fakeContainer struct {
	labels map[string]string
	host   *container.HostConfig
	state  string
}

// fakeEngine is a container engine whose pulls, creates and stops block until the test lets
// them through.
type fakeEngine struct {
	mu           sync.Mutex
	containers   map[string]*fakeContainer
	images       map[string]bool
	pulls, stops map[string]int
	gates        map[string]chan struct{}
	waiting      map[string]bool
	pullFailures map[string]int
	doubleBooked []string
	// stoppingState is what a container inside its stop grace is listed as: podman says
	// "stopping", docker keeps saying "running".
	stoppingState string
}

func newFakeEngine() *fakeEngine {
	return &fakeEngine{
		containers: map[string]*fakeContainer{}, images: map[string]bool{},
		pulls: map[string]int{}, stops: map[string]int{},
		gates: map[string]chan struct{}{}, waiting: map[string]bool{}, pullFailures: map[string]int{},
		stoppingState: "stopping",
	}
}

func (f *fakeEngine) blockedIn(call string) func() bool {
	return func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.waiting[call]
	}
}

// block makes the named call ("pull <image>", "create <container>", "stop <container>") wait
// until the returned func is called or the call's context ends.
func (f *fakeEngine) block(call string) (release func()) {
	gate := make(chan struct{})
	f.mu.Lock()
	f.gates[call] = gate
	f.mu.Unlock()
	return sync.OnceFunc(func() { close(gate) })
}

func (f *fakeEngine) wait(ctx context.Context, call string) error {
	f.mu.Lock()
	gate := f.gates[call]
	f.waiting[call] = gate != nil
	f.mu.Unlock()
	if gate == nil {
		return nil
	}
	select {
	case <-gate:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *fakeEngine) count(counter map[string]int, key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return counter[key]
}

func (f *fakeEngine) state(experimentID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.containers[containerName(experimentID)]; c != nil {
		return c.state
	}
	return ""
}

func (f *fakeEngine) devices(experimentID string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	if c := f.containers[containerName(experimentID)]; c != nil {
		for _, d := range c.host.Devices {
			out = append(out, d.PathOnHost)
		}
	}
	return out
}

func (f *fakeEngine) ContainerList(context.Context, client.ContainerListOptions) (client.ContainerListResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out client.ContainerListResult
	for name, c := range f.containers {
		out.Items = append(out.Items, container.Summary{
			ID: name, Names: []string{"/" + name}, Labels: c.labels, State: container.ContainerState(c.state),
		})
	}
	return out, nil
}

func (f *fakeEngine) ContainerInspect(_ context.Context, id string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.containers[id]
	if c == nil {
		return client.ContainerInspectResult{}, fmt.Errorf("container %s: %w", id, errdefs.ErrNotFound)
	}
	return client.ContainerInspectResult{Container: container.InspectResponse{HostConfig: c.host, State: &container.State{}}}, nil
}

func (f *fakeEngine) ContainerCreate(ctx context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	if err := f.wait(ctx, "create "+options.Name); err != nil {
		return client.ContainerCreateResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range options.HostConfig.Devices {
		for name, other := range f.containers {
			if other.state == "exited" {
				continue
			}
			for _, held := range other.host.Devices {
				if held.PathOnHost == d.PathOnHost {
					f.doubleBooked = append(f.doubleBooked, fmt.Sprintf("%s given to %s while %s (%s) holds it", d.PathOnHost, options.Name, name, other.state))
				}
			}
		}
	}
	f.containers[options.Name] = &fakeContainer{labels: options.Config.Labels, host: options.HostConfig, state: "created"}
	return client.ContainerCreateResult{ID: options.Name}, nil
}

func (f *fakeEngine) ContainerStart(_ context.Context, id string, _ client.ContainerStartOptions) (client.ContainerStartResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.containers[id].state = "running"
	return client.ContainerStartResult{}, nil
}

func (f *fakeEngine) ContainerStop(ctx context.Context, id string, _ client.ContainerStopOptions) (client.ContainerStopResult, error) {
	f.mu.Lock()
	c := f.containers[id]
	if c == nil {
		f.mu.Unlock()
		return client.ContainerStopResult{}, fmt.Errorf("container %s: %w", id, errdefs.ErrNotFound)
	}
	f.stops[id]++
	before := c.state
	c.state = f.stoppingState
	f.mu.Unlock()

	err := f.wait(ctx, "stop "+id)
	f.mu.Lock()
	defer f.mu.Unlock()
	if err != nil {
		c.state = before
		return client.ContainerStopResult{}, err
	}
	c.state = "exited"
	return client.ContainerStopResult{}, nil
}

func (f *fakeEngine) ContainerRemove(_ context.Context, id string, _ client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.containers, id)
	return client.ContainerRemoveResult{}, nil
}

func (f *fakeEngine) ContainerLogs(context.Context, string, client.ContainerLogsOptions) (client.ContainerLogsResult, error) {
	return nil, fmt.Errorf("fake engine keeps no logs")
}

func (f *fakeEngine) ImageInspect(_ context.Context, image string, _ ...client.ImageInspectOption) (client.ImageInspectResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.images[image] {
		return client.ImageInspectResult{}, fmt.Errorf("image %s: %w", image, errdefs.ErrNotFound)
	}
	return client.ImageInspectResult{}, nil
}

type fakePull struct{ client.ImagePullResponse }

func (fakePull) Read([]byte) (int, error) { return 0, io.EOF }
func (fakePull) Close() error             { return nil }

func (f *fakeEngine) ImagePull(ctx context.Context, image string, _ client.ImagePullOptions) (client.ImagePullResponse, error) {
	f.mu.Lock()
	f.pulls[image]++
	fail := f.pullFailures[image] > 0
	if fail {
		f.pullFailures[image]--
	}
	f.mu.Unlock()
	if fail {
		return nil, fmt.Errorf("registry unreachable")
	}
	if err := f.wait(ctx, "pull "+image); err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.images[image] = true
	f.mu.Unlock()
	return fakePull{}, nil
}

func newFakeExecutor(f *fakeEngine, devicePaths ...string) *Executor {
	return &Executor{
		apiURL: "http://example.invalid:8083",
		docker: f,
		probe: func(context.Context) ([]acceleratorDevice, error) {
			var devices []acceleratorDevice
			for _, p := range devicePaths {
				devices = append(devices, acceleratorDevice{DevicePath: p, Labels: []string{fakeAcceleratorType}})
			}
			return devices, nil
		},
	}
}

func fakeJob(id, image string, accelerators int) *domain.Experiment {
	return &domain.Experiment{
		Data: testDataAccess(),
		ID:   id, AgentID: "agent", ProjectID: "project",
		AcceleratorType: fakeAcceleratorType, CapacityTier: domain.CapacityGuaranteed,
		Job: domain.JobSpec{
			Image: image, CPU: "1", Memory: "1Gi", Storage: "1Gi",
			MaxRetries: maxRetriesPtr(0), TerminationGracePeriodSeconds: int64Ptr(5),
			AcceleratorType: fakeAcceleratorType, AcceleratorCount: accelerators,
		},
	}
}

// pass is one reconcile pass as agentloop runs it: compare desired with actual, create, delete,
// reap. It fails the test if the pass waits on an operation in flight.
func pass(t *testing.T, ctx context.Context, e *Executor, desired ...*domain.Experiment) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		byID := map[string]*domain.Experiment{}
		for _, exp := range desired {
			byID[exp.ID] = exp
		}
		actual, err := e.ListManagedJobs(ctx)
		if err != nil {
			t.Errorf("ListManagedJobs: %v", err)
			return
		}
		actualSet := map[string]bool{}
		for _, id := range actual {
			if actualSet[id] {
				t.Errorf("ListManagedJobs lists %s twice", id)
			}
			actualSet[id] = true
		}
		for id, exp := range byID {
			if !actualSet[id] {
				if err := e.CreateWorkload(ctx, exp); err != nil {
					t.Logf("create workload %s: %v", id, err)
				}
				continue
			}
			matches, err := e.WorkloadMatchesDesired(ctx, exp)
			if err != nil {
				t.Errorf("WorkloadMatchesDesired %s: %v", id, err)
			} else if !matches {
				t.Errorf("workload %s drifted from an unchanged desired spec", id)
			}
		}
		for id := range actualSet {
			if byID[id] == nil {
				if err := e.DeleteWorkload(ctx, id, true); err != nil {
					t.Errorf("DeleteWorkload %s: %v", id, err)
				}
			}
		}
		if err := e.ReapTerminal(ctx, byID); err != nil {
			t.Errorf("ReapTerminal: %v", err)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the reconcile pass waited on an operation in flight")
	}
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func idle(e *Executor, experimentID string) func() bool {
	return func() bool {
		_, busy := e.operations()[experimentID]
		return !busy
	}
}

func freeAccelerators(t *testing.T, ctx context.Context, e *Executor) int64 {
	t.Helper()
	available, _, _, _, err := e.GetLiveAcceleratorCapacitySnapshot(ctx)
	if err != nil {
		t.Fatalf("GetLiveAcceleratorCapacitySnapshot: %v", err)
	}
	return available[fakeAcceleratorType]
}

func TestABlockedPullDelaysNoOtherStartOrStop(t *testing.T) {
	f := newFakeEngine()
	e := newFakeExecutor(f)
	ctx := t.Context()
	releasePull := f.block("pull slow")
	slow, fast, leaving := fakeJob("slow", "slow", 0), fakeJob("fast", "fast", 0), fakeJob("leaving", "fast", 0)

	pass(t, ctx, e, leaving)
	eventually(t, "the leaving job runs", func() bool { return f.state("leaving") == "running" })
	eventually(t, "the leaving job's start has returned", idle(e, "leaving"))

	pass(t, ctx, e, slow, fast)
	eventually(t, "the fast job runs beside the blocked pull", func() bool { return f.state("fast") == "running" })
	eventually(t, "the leaving job is stopped behind the blocked pull", func() bool { return f.state("leaving") == "exited" })
	if got := f.state("slow"); got != "" {
		t.Fatalf("slow job has a %s container while its pull is blocked", got)
	}

	releasePull()
	eventually(t, "the slow job runs once its pull is through", func() bool { return f.state("slow") == "running" })
}

func TestAJobInFlightGetsNoSecondOperation(t *testing.T) {
	for _, stoppingState := range []string{"stopping", "running"} {
		t.Run("engine lists a stopping container as "+stoppingState, func(t *testing.T) {
			testAJobInFlightGetsNoSecondOperation(t, stoppingState)
		})
	}
}

func testAJobInFlightGetsNoSecondOperation(t *testing.T, stoppingState string) {
	f := newFakeEngine()
	f.stoppingState = stoppingState
	e := newFakeExecutor(f)
	ctx := t.Context()
	releasePull := f.block("pull slow")
	defer releasePull()
	slow, stopping := fakeJob("slow", "slow", 0), fakeJob("stopping", "fast", 0)

	pass(t, ctx, e, stopping)
	eventually(t, "the job to be stopped runs", func() bool { return f.state("stopping") == "running" })
	eventually(t, "its start has returned", idle(e, "stopping"))

	releaseStop := f.block("stop " + containerName("stopping"))
	defer releaseStop()
	for range 5 {
		pass(t, ctx, e, slow)
	}
	if got := f.count(f.pulls, "slow"); got != 1 {
		t.Fatalf("%d pulls started for one job in flight, want 1", got)
	}
	if got := f.count(f.stops, containerName("stopping")); got != 1 {
		t.Fatalf("%d stops started for one job in flight, want 1", got)
	}
	// Podman's "stopping" is neither running nor created: reaping it now would kill it inside
	// its grace.
	if got := f.state("stopping"); got != stoppingState {
		t.Fatalf("container being stopped is %q, want it left alone as %s", got, stoppingState)
	}

	ids, err := e.ListManagedJobsForStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "stopping" {
		t.Fatalf("status covers %v, want only the job being stopped: a pulling job has no container to report", ids)
	}
	status, err := e.PollJobStatus(ctx, "stopping")
	if err != nil {
		t.Fatal(err)
	}
	if status.Phase == workload.JobPhaseGone {
		t.Fatal("a job still being stopped is reported as gone")
	}

	releaseStop()
	eventually(t, "the stop has returned", idle(e, "stopping"))
	pass(t, ctx, e, slow)
	eventually(t, "the stopped container is reaped", func() bool { return f.state("stopping") == "" })
}

func TestADeviceIsNeverHeldByTwoJobs(t *testing.T) {
	f := newFakeEngine()
	e := newFakeExecutor(f, "/dev/fake/0", "/dev/fake/1")
	ctx := t.Context()
	releasePull := f.block("pull slow")
	defer releasePull()
	pulling, running, third := fakeJob("pulling", "slow", 1), fakeJob("running", "fast", 1), fakeJob("third", "fast", 1)

	pass(t, ctx, e, pulling, running)
	eventually(t, "the second job runs beside the pull", func() bool { return f.state("running") == "running" })
	eventually(t, "its start has returned", idle(e, "running"))
	if free := freeAccelerators(t, ctx, e); free != 0 {
		t.Fatalf("%d accelerators reported free with one pulling and one running on a two-device node", free)
	}

	// Both devices are spoken for, one of them by a job that has no container yet.
	pass(t, ctx, e, pulling, running, third)
	if got := f.state("third"); got != "" {
		t.Fatalf("third job got a %s container while both devices are held", got)
	}

	// A device stays held through the stop's grace, although the engine no longer calls the
	// container running.
	releaseStop := f.block("stop " + containerName("running"))
	defer releaseStop()
	pass(t, ctx, e, pulling, third)
	pass(t, ctx, e, pulling, third)
	if got := f.state("third"); got != "" {
		t.Fatalf("third job got a %s container while the device is still being vacated", got)
	}
	if free := freeAccelerators(t, ctx, e); free != 0 {
		t.Fatalf("%d accelerators reported free while one job pulls and another is inside its stop grace", free)
	}

	releaseStop()
	eventually(t, "the stop has returned", idle(e, "running"))
	pass(t, ctx, e, pulling, third)
	eventually(t, "the third job runs on the vacated device", func() bool { return f.state("third") == "running" })

	releasePull()
	eventually(t, "the pulling job runs", func() bool { return f.state("pulling") == "running" })
	eventually(t, "every start has returned", func() bool { return len(e.operations()) == 0 })
	pass(t, ctx, e, pulling, third)

	a, b := f.devices("pulling"), f.devices("third")
	if len(a) != 1 || len(b) != 1 || a[0] == b[0] {
		t.Fatalf("pulling job holds %v and third job holds %v, want one distinct device each", a, b)
	}
	if len(f.doubleBooked) > 0 {
		t.Fatalf("device handed out twice: %v", f.doubleBooked)
	}
}

func TestAJobCancelledMidPullEndsStopped(t *testing.T) {
	f := newFakeEngine()
	e := newFakeExecutor(f, "/dev/fake/0")
	ctx := t.Context()
	defer f.block("pull slow")()

	pass(t, ctx, e, fakeJob("cancelled", "slow", 1))
	if free := freeAccelerators(t, ctx, e); free != 0 {
		t.Fatalf("%d accelerators free while the job pulls, want its device taken before the pull", free)
	}
	pass(t, ctx, e)
	eventually(t, "the cancelled pull has returned", idle(e, "cancelled"))
	pass(t, ctx, e)

	if got := f.state("cancelled"); got != "" {
		t.Fatalf("cancelled job has a %s container", got)
	}
	if got := f.count(f.pulls, "slow"); got != 1 {
		t.Fatalf("%d pulls for a job cancelled during its first, want 1", got)
	}
	if free := freeAccelerators(t, ctx, e); free != 1 {
		t.Fatalf("%d accelerators free after the cancelled pull, want its device back", free)
	}
}

// The cancel can land too late to stop the container coming up. Then the engine shows a
// container nobody wants, and the ordinary pass stops and reaps it.
func TestAJobCancelledAfterItsPullIsStoppedByTheNextPass(t *testing.T) {
	f := newFakeEngine()
	e := newFakeExecutor(f)
	ctx := t.Context()
	releaseCreate := f.block("create " + containerName("late"))
	defer releaseCreate()

	pass(t, ctx, e, fakeJob("late", "fast", 0))
	eventually(t, "the pull is through", f.blockedIn("create "+containerName("late")))
	pass(t, ctx, e)
	releaseCreate()
	eventually(t, "the start has returned", idle(e, "late"))
	if got := f.state("late"); got != "running" {
		t.Fatalf("container is %q, want it started: a created container is never started later", got)
	}

	pass(t, ctx, e)
	eventually(t, "the unwanted container is stopped", func() bool { return f.state("late") == "exited" })
	eventually(t, "the stop has returned", idle(e, "late"))
	pass(t, ctx, e)
	eventually(t, "the stopped container is reaped", func() bool { return f.state("late") == "" })
}

func TestAFailedPullIsRetriedByTheNextPass(t *testing.T) {
	f := newFakeEngine()
	f.pullFailures["flaky"] = 1
	e := newFakeExecutor(f)
	ctx := t.Context()
	job := fakeJob("retried", "flaky", 0)

	pass(t, ctx, e, job)
	eventually(t, "the failed pull has returned", idle(e, "retried"))
	if got := f.state("retried"); got != "" {
		t.Fatalf("job has a %s container after a failed pull", got)
	}
	pass(t, ctx, e, job)
	eventually(t, "the retry runs the job", func() bool { return f.state("retried") == "running" })
	if got := f.count(f.pulls, "flaky"); got != 2 {
		t.Fatalf("%d pulls, want the failed one and one retry", got)
	}
}

func TestShutdownCancelsOperationsAndWaitsForThem(t *testing.T) {
	f := newFakeEngine()
	e := newFakeExecutor(f)
	ctx, shutdown := context.WithCancel(t.Context())
	defer shutdown()
	defer f.block("pull slow")()
	pulling, stopping := fakeJob("pulling", "slow", 0), fakeJob("stopping", "fast", 0)

	pass(t, ctx, e, stopping)
	eventually(t, "the job to be stopped runs", func() bool { return f.state("stopping") == "running" })
	eventually(t, "its start has returned", idle(e, "stopping"))
	defer f.block("stop " + containerName("stopping"))()
	pass(t, ctx, e, pulling)
	eventually(t, "the stop is under way", f.blockedIn("stop "+containerName("stopping")))
	eventually(t, "the pull is under way", f.blockedIn("pull slow"))

	shutdown()
	waited := make(chan struct{})
	go func() { e.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return after shutdown: an operation was not cancelled")
	}
	if n := len(e.operations()); n != 0 {
		t.Fatalf("%d operations still in flight after Wait", n)
	}

	// A restarted agent knows of no operation in flight and converges from the engine alone.
	f.mu.Lock()
	f.gates = map[string]chan struct{}{}
	f.mu.Unlock()
	restarted := newFakeExecutor(f)
	ctx = t.Context()
	pass(t, ctx, restarted, pulling)
	eventually(t, "the interrupted pull is run again", func() bool { return f.state("pulling") == "running" })
	eventually(t, "the interrupted stop is run again", func() bool { return f.state("stopping") == "exited" })
	eventually(t, "the restarted agent is idle", func() bool { return len(restarted.operations()) == 0 })
}
