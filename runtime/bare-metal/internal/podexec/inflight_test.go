package podexec

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/registry"
	"github.com/moby/moby/client"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/agentexec"
)

const fakeAcceleratorType = "example.com/chip=fake"

type fakeContainer struct {
	labels map[string]string
	host   *container.HostConfig
	state  string
	exit   int
	// written is the size of the writable layer, reported only to a list that asks for sizes.
	written int64
	// stateless makes an inspect answer without a state.
	stateless bool
}

// fakeEngine is a container engine whose pulls, creates and stops block until the test lets
// them through.
type fakeEngine struct {
	mu           sync.Mutex
	containers   map[string]*fakeContainer
	images       map[string]bool
	pulls, stops map[string]int
	stopGraces   map[string]*int
	// deadlines is, per kind of call, whether the last one carried a deadline.
	deadlines    map[string]bool
	gates        map[string]chan struct{}
	waiting      map[string]bool
	pullFailures map[string]int
	// pullAuths is the registry login each image was last pulled with.
	pullAuths    map[string]string
	doubleBooked []string
	// stoppingState is what a container inside its stop grace is listed as: podman says
	// "stopping", docker keeps saying "running".
	stoppingState string
	// sizedLists counts the lists that asked for sizes.
	sizedLists int
}

func newFakeEngine() *fakeEngine {
	return &fakeEngine{
		containers: map[string]*fakeContainer{}, images: map[string]bool{},
		pulls: map[string]int{}, stops: map[string]int{}, stopGraces: map[string]*int{},
		deadlines: map[string]bool{},
		gates:     map[string]chan struct{}{}, waiting: map[string]bool{}, pullFailures: map[string]int{},
		pullAuths:     map[string]string{},
		stoppingState: "stopping",
	}
}

func (f *fakeEngine) sawDeadline(ctx context.Context, kind string) {
	_, has := ctx.Deadline()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deadlines[kind] = has
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

func (f *fakeEngine) ContainerList(ctx context.Context, options client.ContainerListOptions) (client.ContainerListResult, error) {
	f.sawDeadline(ctx, "list")
	f.mu.Lock()
	defer f.mu.Unlock()
	var out client.ContainerListResult
	if options.Size {
		f.sizedLists++
	}
	for name, c := range f.containers {
		summary := container.Summary{
			ID: name, Names: []string{"/" + name}, Labels: c.labels, State: container.ContainerState(c.state),
		}
		if options.Size {
			summary.SizeRw = c.written
		}
		out.Items = append(out.Items, summary)
	}
	return out, nil
}

func (f *fakeEngine) ContainerInspect(ctx context.Context, id string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	f.sawDeadline(ctx, "inspect")
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.containers[id]
	if c == nil {
		return client.ContainerInspectResult{}, fmt.Errorf("container %s: %w", id, errdefs.ErrNotFound)
	}
	if c.stateless {
		return client.ContainerInspectResult{Container: container.InspectResponse{HostConfig: c.host}}, nil
	}
	return client.ContainerInspectResult{Container: container.InspectResponse{HostConfig: c.host, State: &container.State{ExitCode: c.exit}}}, nil
}

func (f *fakeEngine) ContainerCreate(ctx context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	f.sawDeadline(ctx, "create")
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

func (f *fakeEngine) ContainerStart(ctx context.Context, id string, _ client.ContainerStartOptions) (client.ContainerStartResult, error) {
	f.sawDeadline(ctx, "start")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.containers[id].state = "running"
	return client.ContainerStartResult{}, nil
}

func (f *fakeEngine) ContainerStop(ctx context.Context, id string, options client.ContainerStopOptions) (client.ContainerStopResult, error) {
	f.sawDeadline(ctx, "stop")
	f.mu.Lock()
	c := f.containers[id]
	if c == nil {
		f.mu.Unlock()
		return client.ContainerStopResult{}, fmt.Errorf("container %s: %w", id, errdefs.ErrNotFound)
	}
	f.stops[id]++
	f.stopGraces[id] = options.Timeout
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

func (f *fakeEngine) ContainerRemove(ctx context.Context, id string, _ client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	f.sawDeadline(ctx, "remove")
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.containers, id)
	return client.ContainerRemoveResult{}, nil
}

func (f *fakeEngine) ContainerLogs(ctx context.Context, _ string, _ client.ContainerLogsOptions) (client.ContainerLogsResult, error) {
	f.sawDeadline(ctx, "logs")
	return nil, fmt.Errorf("fake engine keeps no logs")
}

func (f *fakeEngine) ImageInspect(ctx context.Context, image string, _ ...client.ImageInspectOption) (client.ImageInspectResult, error) {
	f.sawDeadline(ctx, "image inspect")
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.images[image] {
		return client.ImageInspectResult{}, fmt.Errorf("image %s: %w", image, errdefs.ErrNotFound)
	}
	return client.ImageInspectResult{}, nil
}

type fakePull struct{ client.ImagePullResponse }

func (fakePull) Wait(context.Context) error { return nil }

func (f *fakeEngine) ImagePull(ctx context.Context, image string, options client.ImagePullOptions) (client.ImagePullResponse, error) {
	f.sawDeadline(ctx, "pull")
	f.mu.Lock()
	f.pulls[image]++
	f.pullAuths[image] = options.RegistryAuth
	fail := f.pullFailures[image] > 0
	if fail {
		f.pullFailures[image]--
	}
	f.mu.Unlock()
	if fail {
		return nil, fmt.Errorf("registry unreachable")
	}
	if image == "absent" {
		return nil, fmt.Errorf("manifest unknown: %w", errdefs.ErrNotFound)
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
		docker: bounded{f},
		life:   context.Background(),
		probe: func(context.Context) ([]acceleratorDevice, error) {
			var devices []acceleratorDevice
			for _, p := range devicePaths {
				devices = append(devices, acceleratorDevice{DevicePath: p, Labels: []string{fakeAcceleratorType}})
			}
			return devices, nil
		},
	}
}

func fakeJob(id, image string, accelerators int) *clusterapi.DesiredJob {
	return &clusterapi.DesiredJob{
		Data: testDataAccess(), Token: "test-job-token", GitURL: "https://api.test/git/pe-1.git",
		ID: id, AgentID: "agent", ProjectID: "project",
		AcceleratorType: fakeAcceleratorType, CapacityTier: clusterapi.CapacityGuaranteed,
		Job: clusterapi.JobSpec{
			Image: image, CPU: "1", Memory: "1Gi", Storage: "1Gi",
			MaxRetries: maxRetriesPtr(0), TerminationGracePeriodSeconds: int64Ptr(5),
			AcceleratorType: fakeAcceleratorType, AcceleratorCount: accelerators,
		},
	}
}

// pass is one reconcile pass as agentloop runs it, under a context that ends with it: compare
// desired with actual, create, delete, reap. It fails the test if the pass waits on an operation
// in flight.
func pass(t *testing.T, ctx context.Context, e *Executor, desired ...*clusterapi.DesiredJob) {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		byID := map[string]*clusterapi.DesiredJob{}
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
				if err := e.CreateWorkload(ctx, exp, nil); err != nil {
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
	// A pass hands each operation to its own goroutine and returns, so the engine is asked
	// some time after the last pass has ended.
	eventually(t, "the pull has reached the engine", f.blockedIn("pull slow"))
	eventually(t, "the stop has reached the engine", f.blockedIn("stop "+containerName("stopping")))
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
	if len(ids) != 2 || ids[0] != "stopping" || ids[1] != "slow" {
		t.Fatalf("status covers %v, want the job being stopped and the job pulling", ids)
	}
	status, err := e.PollJobStatus(ctx, "stopping")
	if err != nil {
		t.Fatal(err)
	}
	if status.Phase == clusterapi.JobPhaseGone {
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

// Placement counted the devices of the submitted job and the container was built from the
// resolved one, so a job whose two counts differ was refused for a placement of the wrong size.
func TestAJobIsPlacedOnTheDevicesItsResolvedSpecCounts(t *testing.T) {
	f := newFakeEngine()
	e := newFakeExecutor(f, "/dev/fake/0", "/dev/fake/1")
	ctx := t.Context()
	job := fakeJob("resolved", "fast", 1)
	resolved := job.Job
	resolved.AcceleratorCount = 2
	job.ResolvedJob = &resolved

	placement, err := e.resolvePlacementFor(ctx, job)
	if err != nil || len(placement.DevicePaths) != 2 {
		t.Fatalf("placement = %v (err %v), want the two devices the resolved job counts", placement.DevicePaths, err)
	}
	if _, err := e.BuildContainerSpec(job, placement); err != nil {
		t.Fatalf("BuildContainerSpec refused the placement made for the same job: %v", err)
	}
}

// An NVIDIA device is handed over as a device request, which the engine keeps apart from the
// plain mappings. Read back from the mappings alone it looked free and was given to every job.
func TestADevicePassedByRequestIsReadBackAsHeld(t *testing.T) {
	f := newFakeEngine()
	e := newFakeExecutor(f, "/dev/nvidia0", "/dev/nvidia1", "/dev/nvidiactl")
	ctx := t.Context()
	first, second := fakeJob("first", "fast", 1), fakeJob("second", "fast", 1)

	pass(t, ctx, e, first)
	eventually(t, "the first job runs", func() bool { return f.state("first") == "running" })
	eventually(t, "its start has returned", idle(e, "first"))
	pass(t, ctx, e, first, second)
	eventually(t, "the second job runs", func() bool { return f.state("second") == "running" })
	eventually(t, "its start has returned", idle(e, "second"))
	pass(t, ctx, e, first, second)

	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []string
	for _, id := range []string{"first", "second"} {
		host := f.containers[containerName(id)].host
		if len(host.Devices) != 0 || len(host.DeviceRequests) != 1 || host.DeviceRequests[0].Driver != "nvidia" || len(host.DeviceRequests[0].DeviceIDs) != 1 {
			t.Fatalf("job %s was given mappings %v and requests %+v, want one nvidia request naming one device", id, host.Devices, host.DeviceRequests)
		}
		ids = append(ids, host.DeviceRequests[0].DeviceIDs[0])
	}
	if !slices.Equal(ids, []string{"0", "1"}) {
		t.Fatalf("the two jobs were given devices %v, want 0 and 1", ids)
	}
	if mappings, requests := passDevices([]string{"/dev/nvidiactl", "/dev/fake/0"}); len(mappings) != 2 || len(requests) != 0 {
		t.Fatalf("paths that are no NVIDIA device were passed as %v and %+v, want two plain mappings", mappings, requests)
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
	if ids, err := e.ListManagedJobsForStatus(ctx); err != nil || !slices.Contains(ids, "retried") {
		t.Fatalf("status lists %v (err %v), want the job between its failed pull and the retry", ids, err)
	}
	if status, err := e.PollJobStatus(ctx, "retried"); err != nil || status.Phase != clusterapi.JobPhasePending {
		t.Fatalf("status = %+v (err %v), want pending", status, err)
	}
	reason, message, _, _, err := e.PollPhaseDetail(ctx, "retried", 0)
	if err != nil || reason != "" || message != "pull image flaky: registry unreachable" {
		t.Fatalf("phase detail = %q %q (err %v), want the image and the engine's whole error as the message and no reason that ends the job", reason, message, err)
	}
	pass(t, ctx, e, job)
	eventually(t, "the retry runs the job", func() bool { return f.state("retried") == "running" })
	if got := f.count(f.pulls, "flaky"); got != 2 {
		t.Fatalf("%d pulls, want the failed one and one retry", got)
	}
	eventually(t, "the start has returned", idle(e, "retried"))
	if _, message, _, _, _ := e.PollPhaseDetail(ctx, "retried", 0); message != "" {
		t.Fatalf("phase detail still says %q after the pull succeeded", message)
	}
}

func TestAnImageThatCanNeverBePulledMakesTheJobUnrunnable(t *testing.T) {
	f := newFakeEngine()
	e := newFakeExecutor(f)
	ctx := t.Context()
	job := fakeJob("hopeless", "absent", 0)

	pass(t, ctx, e, job)
	eventually(t, "the failed pull has returned", idle(e, "hopeless"))
	err := e.CreateWorkload(ctx, job, nil)
	var unrunnable *agentexec.Unrunnable
	if !errors.As(err, &unrunnable) || unrunnable.Reason != clusterapi.PhaseReasonImagePullFailed || !strings.Contains(unrunnable.Why, "manifest unknown") {
		t.Fatalf("CreateWorkload = %v, want an Unrunnable for a failed image pull carrying the engine's error", err)
	}
	if got := f.count(f.pulls, "absent"); got != 1 {
		t.Fatalf("%d pulls, want one: an image that is not there is not pulled again", got)
	}
	if ids, err := e.ListManagedJobsForStatus(ctx); err != nil || len(ids) != 0 {
		t.Fatalf("status lists %v (err %v), want nothing: the loop reports an unrunnable job itself", ids, err)
	}

	pass(t, ctx, e)
	pass(t, ctx, e, job)
	eventually(t, "the job is pulled again once it was gone from desired state", func() bool { return f.count(f.pulls, "absent") == 2 })
	eventually(t, "that pull has returned", idle(e, "hopeless"))
}

func TestShutdownCancelsOperationsAndWaitsForThem(t *testing.T) {
	f := newFakeEngine()
	e := newFakeExecutor(f)
	ctx, shutdown := context.WithCancel(t.Context())
	defer shutdown()
	e.life = ctx
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

// A k8s pod pulling its image is pending, bound and has no reason. The job watcher exempts that
// from the stuck-pending timeout, so a pull longer than the timeout must report the same here.
func TestAJobPullingItsImageIsReportedPendingAndPlaced(t *testing.T) {
	f := newFakeEngine()
	e := newFakeExecutor(f)
	ctx := t.Context()
	release := f.block("pull slow")
	defer release()
	job := fakeJob("pulling", "slow", 0)
	job.AttemptCount = 3

	pass(t, ctx, e, job)
	eventually(t, "the pull is under way", f.blockedIn("pull slow"))

	ids, err := e.ListManagedJobsForStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "pulling" {
		t.Fatalf("status lists %v while the image is pulled, want the pulling job", ids)
	}
	status, err := e.PollJobStatus(ctx, "pulling")
	if err != nil {
		t.Fatal(err)
	}
	if status.Phase != clusterapi.JobPhasePending || status.Attempt != 3 {
		t.Fatalf("status = %s attempt %d, want pending attempt 3", status.Phase, status.Attempt)
	}
	reason, message, _, scheduledNodes, err := e.PollPhaseDetail(ctx, "pulling", status.Attempt)
	if err != nil {
		t.Fatal(err)
	}
	if reason != "" || message != "" || scheduledNodes != 1 {
		t.Fatalf("phase detail = (%q, %q, %d nodes), want no reason and 1 node", reason, message, scheduledNodes)
	}

	release()
	eventually(t, "the start has returned", idle(e, "pulling"))
	ids, err = e.ListManagedJobsForStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 {
		t.Fatalf("status lists %v once the container runs, want the job once", ids)
	}

	// Cancelled mid-pull, the job has neither container nor operation and reads as gone.
	defer f.block("pull other")()
	pass(t, ctx, e, fakeJob("dropped", "other", 0), job)
	eventually(t, "the second pull is under way", f.blockedIn("pull other"))
	pass(t, ctx, e, job)
	eventually(t, "the cancelled pull has returned", idle(e, "dropped"))
	if status, err := e.PollJobStatus(ctx, "dropped"); err != nil || status.Phase != clusterapi.JobPhaseGone {
		t.Fatalf("status of a job with no container and no operation = %s, %v, want gone", status.Phase, err)
	}
}

func TestAContainerWithNoReadableGraceIsStoppedAndItsDeleteIsAnError(t *testing.T) {
	f := newFakeEngine()
	e := newFakeExecutor(f, "/dev/fake/0")
	ctx := t.Context()
	labelled, unlabelled := fakeJob("labelled", "fast", 0), fakeJob("unlabelled", "fast", 1)

	pass(t, ctx, e, labelled, unlabelled)
	eventually(t, "both jobs run", func() bool {
		return f.state("labelled") == "running" && f.state("unlabelled") == "running"
	})
	eventually(t, "both starts have returned", func() bool { return len(e.operations()) == 0 })
	f.mu.Lock()
	delete(f.containers[containerName("unlabelled")].labels, LabelGraceSeconds)
	f.mu.Unlock()

	if err := e.DeleteWorkload(ctx, "labelled", false); err != nil {
		t.Fatalf("DeleteWorkload of a container with its grace: %v", err)
	}
	err := e.DeleteWorkload(ctx, "unlabelled", false)
	if err == nil || !strings.Contains(err.Error(), containerName("unlabelled")) || !strings.Contains(err.Error(), "no readable shutdown grace") {
		t.Fatalf("DeleteWorkload = %v, want an error naming the container and its unreadable grace", err)
	}
	eventually(t, "both jobs are stopped", func() bool {
		return f.state("labelled") != "running" && f.state("unlabelled") != "running"
	})
	eventually(t, "both stops have returned", func() bool { return len(e.operations()) == 0 })

	f.mu.Lock()
	withLabel, withoutLabel := f.stopGraces[containerName("labelled")], f.stopGraces[containerName("unlabelled")]
	f.mu.Unlock()
	if withLabel == nil || *withLabel != 5 {
		t.Fatalf("labelled container stopped with grace %v, want its own 5 seconds", withLabel)
	}
	if withoutLabel != nil {
		t.Fatalf("container with no grace label stopped with grace %d, want the engine default", *withoutLabel)
	}
	if free := freeAccelerators(t, ctx, e); free != 1 {
		t.Fatalf("%d accelerators free after the stop, want the device back", free)
	}
}

// A hung engine socket must fail a quick call, and must never cut a pull or a stop grace short.
func TestQuickEngineCallsCarryADeadlineAndSlowOnesDoNot(t *testing.T) {
	f := newFakeEngine()
	e := newFakeExecutor(f, "/dev/fake/0")
	ctx := t.Context()
	job := fakeJob("job", "image", 1)

	pass(t, ctx, e, job)
	eventually(t, "the job runs", func() bool { return f.state("job") == "running" })
	eventually(t, "its start has returned", idle(e, "job"))
	if _, err := e.FetchLogTail(ctx, "job", 10); err == nil {
		t.Fatal("the fake engine keeps no logs, want its error")
	}
	pass(t, ctx, e)
	eventually(t, "the job is stopped", func() bool { return f.state("job") != "running" })
	eventually(t, "its stop has returned", idle(e, "job"))
	pass(t, ctx, e)
	eventually(t, "the container is reaped", func() bool { return f.state("job") == "" })
	eventually(t, "the reap has returned", idle(e, "job"))

	f.mu.Lock()
	defer f.mu.Unlock()
	for kind, want := range map[string]bool{
		"list": true, "inspect": true, "image inspect": true, "create": true, "start": true, "remove": true, "logs": true,
		"pull": false, "stop": false,
	} {
		got, called := f.deadlines[kind]
		if !called {
			t.Errorf("the job's life made no %s call", kind)
		} else if got != want {
			t.Errorf("%s call carried a deadline: %v, want %v", kind, got, want)
		}
	}
}

// A container holds its cores, memory and device for as long as it exists with its command not
// ended, whatever the engine calls its state. Counted only while running or created, a paused or
// dead one's device read as free and went to a second job. One paused or restarting is still the
// pass's to compare and stop; one whose command has ended holds nothing.
func TestAContainerHoldsWhatItRequestsInEveryStateButExited(t *testing.T) {
	for state, actual := range map[string]bool{"paused": true, "restarting": true, "dead": false, "removing": false, "hibernating": false} {
		t.Run(state, func(t *testing.T) {
			f := newFakeEngine()
			e := newFakeExecutor(f, "/dev/fake/0", "/dev/fake/1")
			ctx := t.Context()
			held, second := fakeJob("held", "fast", 1), fakeJob("second", "fast", 1)

			pass(t, ctx, e, held)
			eventually(t, "the job runs", func() bool { return f.state("held") == "running" })
			eventually(t, "its start has returned", idle(e, "held"))
			f.mu.Lock()
			f.containers[containerName("held")].state = state
			f.mu.Unlock()

			available, total, err := e.GetLiveCPUCapacity(ctx)
			if err != nil || available != total-1 {
				t.Fatalf("CPU available = %v of %v (err %v), want the container's core held", available, total, err)
			}
			if free := freeAccelerators(t, ctx, e); free != 1 {
				t.Fatalf("%d accelerators free, want 1: the container holds the other", free)
			}
			jobs, err := e.ListManagedJobs(ctx)
			if err != nil || slices.Contains(jobs, "held") != actual {
				t.Fatalf("actual jobs = %v (err %v), want the container listed: %v", jobs, err, actual)
			}

			pass(t, ctx, e, held, second)
			eventually(t, "the second job runs", func() bool { return f.state("second") == "running" })
			if a, b := f.devices("held"), f.devices("second"); len(a) != 1 || len(b) != 1 || a[0] == b[0] {
				t.Fatalf("the %s job holds %v and the second job %v, want one distinct device each", state, a, b)
			}
			if len(f.doubleBooked) > 0 {
				t.Fatalf("device handed out twice: %v", f.doubleBooked)
			}

			// Its job is over: the container goes, stopped first where its command can still run.
			pass(t, ctx, e, second)
			eventually(t, "the container is gone or stopped", func() bool { return f.state("held") == "" || f.state("held") == "exited" })
			eventually(t, "the operation has returned", idle(e, "held"))
			if stops := f.count(f.stops, containerName("held")); (stops == 1) != actual {
				t.Fatalf("%d stops of a %s container, want a stop: %v", stops, state, actual)
			}
			eventually(t, "its device is free again", func() bool { return freeAccelerators(t, ctx, e) == 1 })
		})
	}
}

// Left out of the capacity read, the container's share read as free while it was held, and a
// second job was booked onto it.
func TestAContainerWithNoReadableRequestsLeavesNothingFreeAndKeepsItsDevice(t *testing.T) {
	f := newFakeEngine()
	e := newFakeExecutor(f, "/dev/fake/0", "/dev/fake/1")
	ctx := t.Context()
	broken, second := fakeJob("broken", "fast", 1), fakeJob("second", "fast", 1)

	pass(t, ctx, e, broken)
	eventually(t, "the job runs", func() bool { return f.state("broken") == "running" })
	eventually(t, "its start has returned", idle(e, "broken"))
	f.mu.Lock()
	delete(f.containers[containerName("broken")].labels, LabelCPUCores)
	f.mu.Unlock()

	available, total, err := e.GetLiveCPUCapacity(ctx)
	if err != nil {
		t.Fatalf("GetLiveCPUCapacity: got = %v, want = nil", err)
	}
	if available != 0 || total == 0 {
		t.Fatalf("CPU available = %v of %v, want none free while what the container holds is unknown", available, total)
	}
	if free := freeAccelerators(t, ctx, e); free != 0 {
		t.Fatalf("%d accelerators free, want none while what the container holds is unknown", free)
	}

	f.mu.Lock()
	delete(f.containers[containerName("broken")].labels, LabelAcceleratorType)
	f.mu.Unlock()
	pass(t, ctx, e, broken, second)
	eventually(t, "the second job runs", func() bool { return f.state("second") == "running" })
	a, b := f.devices("broken"), f.devices("second")
	if len(a) != 1 || len(b) != 1 || a[0] == b[0] {
		t.Fatalf("unreadable job holds %v and second job holds %v, want one distinct device each", a, b)
	}
	if len(f.doubleBooked) > 0 {
		t.Fatalf("device handed out twice: %v", f.doubleBooked)
	}
}

// A job that runs in an environment pulls its image with the reconcile answer's login, a job on a
// public job image with none, and a job in an environment is not pulled at all without a login.
func TestAJobInAnEnvironmentPullsWithTheAnswersLogin(t *testing.T) {
	f := newFakeEngine()
	e := newFakeExecutor(f)
	ctx := t.Context()
	pull := &clusterapi.RegistryLogin{Registry: "europe-west1-docker.pkg.dev", Username: "oauth2accesstoken", Password: "pull-token"}
	inEnvironment := fakeJob("in-environment", "europe-west1-docker.pkg.dev/p/r-environments/environment@sha256:abc", 0)
	inEnvironment.EnvironmentKey = "key-1"

	if err := e.CreateWorkload(ctx, fakeJob("plain", "public-job-image", 0), pull); err != nil {
		t.Fatal(err)
	}
	if err := e.CreateWorkload(ctx, inEnvironment, pull); err != nil {
		t.Fatal(err)
	}
	eventually(t, "both pulls have returned", func() bool { return idle(e, "plain")() && idle(e, "in-environment")() })

	f.mu.Lock()
	plainAuth, environmentAuth := f.pullAuths["public-job-image"], f.pullAuths[inEnvironment.Job.Image]
	f.mu.Unlock()
	if plainAuth != "" {
		t.Fatalf("a public job image was pulled with login %q", plainAuth)
	}
	decoded, err := base64.URLEncoding.DecodeString(environmentAuth)
	if err != nil {
		t.Fatalf("the environment's login %q is not what the engine decodes: %v", environmentAuth, err)
	}
	var login registry.AuthConfig
	if err := json.Unmarshal(decoded, &login); err != nil || login.Username != "oauth2accesstoken" || login.Password != "pull-token" || login.ServerAddress != "europe-west1-docker.pkg.dev" {
		t.Fatalf("the environment was pulled with %+v (%v), want the answer's login", login, err)
	}

	inEnvironment.ID = "without-login"
	if err := e.CreateWorkload(ctx, inEnvironment, nil); err == nil || !strings.Contains(err.Error(), "registry_pull") {
		t.Fatalf("a job in an environment with no login = %v, want an error naming registry_pull", err)
	}
}
