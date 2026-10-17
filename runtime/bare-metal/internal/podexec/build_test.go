package podexec

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/agentexec"
)

func TestResolveHostMounts(t *testing.T) {
	t.Run("nil/empty input resolves to nil", func(t *testing.T) {
		got, err := resolveHostMounts(nil)
		if err != nil || got != nil {
			t.Fatalf("resolveHostMounts(nil) = (%v, %v), want (nil, nil)", got, err)
		}
	})

	t.Run("existing directory resolves through unchanged", func(t *testing.T) {
		dir := t.TempDir()
		got, err := resolveHostMounts(map[string]string{"/data/dataset": dir})
		if err != nil {
			t.Fatalf("resolveHostMounts: unexpected error: %v", err)
		}
		if got["/data/dataset"] != dir {
			t.Fatalf("resolveHostMounts = %v, want /data/dataset -> %s", got, dir)
		}
	})

	t.Run("missing host path fails fast rather than silently mounting nothing", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "does-not-exist")
		_, err := resolveHostMounts(map[string]string{"/data/dataset": missing})
		var unrunnable *agentexec.Unrunnable
		if !errors.As(err, &unrunnable) || !strings.Contains(unrunnable.Why, missing) {
			t.Fatalf("resolveHostMounts: err = %v, want an Unrunnable naming the nonexistent host path", err)
		}
	})

	t.Run("host path that is a file, not a directory, fails", func(t *testing.T) {
		f, err := os.CreateTemp(t.TempDir(), "not-a-dir")
		if err != nil {
			t.Fatalf("setup: %v", err)
		}
		defer f.Close()
		_, err = resolveHostMounts(map[string]string{"/data/dataset": f.Name()})
		var unrunnable *agentexec.Unrunnable
		if !errors.As(err, &unrunnable) {
			t.Fatalf("resolveHostMounts: err = %v, want an Unrunnable for a host path that is a file", err)
		}
	})
}

// A job on a Tenstorrent chip is given hugepages-1Gi by the platform. Started on a node with no
// 1 GiB hugetlbfs mount it died in its driver with nothing in its phase reason.
func TestAJobAskingForWhatTheNodeCannotGiveIsUnrunnable(t *testing.T) {
	present, absent := t.TempDir(), filepath.Join(t.TempDir(), "hugepages-1G")
	reserved, none := filepath.Join(t.TempDir(), "nr_hugepages"), filepath.Join(t.TempDir(), "nr_hugepages")
	for file, pages := range map[string]string{reserved: "8\n", none: "0\n"} {
		if err := os.WriteFile(file, []byte(pages), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	saved := hugepageSizes
	hugepageSizes = map[string]hugepageSize{"hugepages-2Mi": {mount: present, reserved: reserved}, "hugepages-1Gi": {mount: absent, reserved: reserved}}
	defer func() { hugepageSizes = saved }()
	e := &Executor{apiURL: "http://example.invalid:8083"}
	placement := Placement{DevicePaths: []string{"/dev/tenstorrent/0"}}

	for name, tc := range map[string]struct {
		change func(*clusterapi.JobSpec)
		want   string
	}{
		"a hugepage size with no mount on the node": {func(s *clusterapi.JobSpec) {
			s.AcceleratorPodResources = map[string]string{"hugepages-1Gi": "4Gi"}
		}, absent},
		"a pod resource that is no hugepage size": {func(s *clusterapi.JobSpec) {
			s.AcceleratorPodResources = map[string]string{"example.com/thing": "1"}
		}, "accelerator_pod_resources[example.com/thing]"},
		"extra_resources": {func(s *clusterapi.JobSpec) {
			s.ExtraResources = map[string]string{"google.com/tpu": "1"}
		}, "google.com/tpu"},
	} {
		t.Run(name, func(t *testing.T) {
			exp := checkpointWindowExperiment(nil)
			tc.change(&exp.Job)
			_, err := e.BuildContainerSpec(exp, placement)
			var unrunnable *agentexec.Unrunnable
			if !errors.As(err, &unrunnable) || !strings.Contains(unrunnable.Why, tc.want) {
				t.Fatalf("BuildContainerSpec: err = %v, want an Unrunnable naming %q", err, tc.want)
			}
		})
	}

	exp := checkpointWindowExperiment(nil)
	exp.Job.AcceleratorPodResources = map[string]string{"hugepages-2Mi": "64Mi"}
	exp.Job.AcceleratorTolerations = []string{"nvidia.com/gpu"}
	cs, err := e.BuildContainerSpec(exp, placement)
	if err != nil {
		t.Fatalf("BuildContainerSpec refused a hugepage size the node has and a toleration, which asks for nothing: %v", err)
	}
	if len(cs.Mounts) != 1 || cs.Mounts[0] != present {
		t.Fatalf("mounts = %v, want the node's hugetlbfs mount %s", cs.Mounts, present)
	}

	// The mount is there on every Linux host; the pages behind it are the operator's to reserve.
	// With none the job started and died in its driver all the same.
	hugepageSizes["hugepages-2Mi"] = hugepageSize{mount: present, reserved: none}
	_, err = e.BuildContainerSpec(exp, placement)
	var unrunnable *agentexec.Unrunnable
	if !errors.As(err, &unrunnable) || !strings.Contains(unrunnable.Why, "reserved none") || !strings.Contains(unrunnable.Why, none) {
		t.Fatalf("a mount with no pages reserved: err = %v, want an Unrunnable naming the count read", err)
	}
	// A count that cannot be read says nothing about the job.
	hugepageSizes["hugepages-2Mi"] = hugepageSize{mount: present, reserved: absent}
	if _, err = e.BuildContainerSpec(exp, placement); err == nil || errors.As(err, &unrunnable) {
		t.Fatalf("an unreadable count: err = %v, want an error that does not end the job", err)
	}
}

// TestHashContainerSpecDetectsReadOnlyMountsDrift guards against a regression where
// ReadOnlyMounts was omitted from hashContainerSpec's input: reconcile compares this hash
// against the running container's desired-spec label to decide whether to recreate it, so a
// change to a job's desired host mounts (different host path or container target) must change
// the hash, or the running container silently keeps stale/wrong mounts forever.
func TestHashContainerSpecDetectsReadOnlyMountsDrift(t *testing.T) {
	base := containerSpec{
		Name:  "test-container",
		Image: "example/image:latest",
		ReadOnlyMounts: map[string]string{
			"/data/dataset": "/host/dataset-v1",
		},
	}
	changed := base
	changed.ReadOnlyMounts = map[string]string{
		"/data/dataset": "/host/dataset-v2",
	}

	baseHash, err := hashContainerSpec(base)
	if err != nil {
		t.Fatalf("hashContainerSpec(base): unexpected error: %v", err)
	}
	changedHash, err := hashContainerSpec(changed)
	if err != nil {
		t.Fatalf("hashContainerSpec(changed): unexpected error: %v", err)
	}
	if baseHash == changedHash {
		t.Fatal("hashContainerSpec: hash unchanged when ReadOnlyMounts changed — reconcile will never detect this drift")
	}
}

func maxRetriesPtr(v int) *int { return &v }

// The same fact the k8s runtime gets wrong when it reads the experiment total: a container is
// handed job.accelerator_count devices, so that is the only number HYPOTHESISLOOP_ACCELERATOR_COUNT
// can truthfully carry. This runtime is single-node today, which makes per-node and total equal
// by accident -- the assertion pins the source, so the next multi-node change cannot silently
// inherit the k8s bug.
func TestBuildContainerSpecInjectsPerNodeAcceleratorCount(t *testing.T) {
	e := &Executor{
		apiURL: "http://example.invalid:8083",
	}
	exp := &clusterapi.DesiredJob{
		Image: testImage, Launch: "true",
		Data: testDataAccess(), Token: "test-job-token", GitURL: "https://api.test/git/pe-1.git",
		ID: "per-node-count", AgentID: "agent", ProjectID: "project",
		AcceleratorType: "tenstorrent.com/chipArch=blackhole",
		// Deliberately disagreeing with the spec: only reading the spec produces "2".
		AcceleratorCount:       8,
		EstimatedDurationHours: 0.02, CapacityTier: clusterapi.CapacityGuaranteed,
		Job: clusterapi.JobSpec{
			CPU: "2", Memory: "8Gi", Storage: "5Gi",
			MaxRetries: maxRetriesPtr(0), TerminationGracePeriodSeconds: int64Ptr(5), AcceleratorCount: 2,
			AcceleratorType: "tenstorrent.com/chipArch=blackhole",
		},
	}

	cs, err := e.BuildContainerSpec(exp, Placement{DevicePaths: []string{"/dev/tenstorrent/0", "/dev/tenstorrent/1"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := cs.Env["HYPOTHESISLOOP_ACCELERATOR_COUNT"]; got != "2" {
		t.Fatalf("HYPOTHESISLOOP_ACCELERATOR_COUNT = %q, want %q (per node, not the 8-device job total)", got, "2")
	}
}

// The engines hold a name to an alphabet and to no length; the name is no hostname, which the
// engine makes from the container's own id.
func TestAContainerIsNamedByItsWholeExperimentId(t *testing.T) {
	const issued, sameMillisecond = "job-0199c2a4b7d07f3a9c1e5b2d4f608a13", "job-0199c2a4b7d07e11a04c7d9e3b5f2c68"
	longest := strings.Repeat("a", clusterapi.MaxIdentifierLength)
	for _, id := range []string{issued, sameMillisecond, longest} {
		if got := containerName(id); got != containerNamePrefix+id {
			t.Errorf("containerName(%q) = %q", id, got)
		}
	}
	one, other := containerName(strings.Repeat("a", 300)+"1"), containerName(strings.Repeat("a", 300)+"2")
	if len(one) != 200 || len(other) != 200 || one == other {
		t.Errorf("ids past the cut are named %q and %q", one, other)
	}
}

// Every job in every test gets the same durable-data access the control plane really sends,
// because BuildJob refuses to build a job without one: a job with nowhere to save is a job whose
// result cannot outlive it, and that must fail at build time rather than at the end of a run.
func testDataAccess() *clusterapi.DataAccess {
	return &clusterapi.DataAccess{
		URI: "s3://hl-data/pe-1/agent/exp-1/", Shared: "s3://hl-data/pe-1/",
		Endpoint: "http://store.invalid:9000", Region: "us-east-1",
		AccessKeyID: "key", SecretAccessKey: "secret",
	}
}

// The same two variables the k8s runtime injects, from the same place. A checkpoint written by a
// job on a bare node has to be readable by a job the scheduler later places in a k8s cluster, so
// the address a job is handed can depend on desired state and on nothing about its runtime.
func TestBuildContainerSpecInjectsDurableDataAddressFromDesiredState(t *testing.T) {
	e := &Executor{
		apiURL: "http://example.invalid:8083",
	}
	exp := &clusterapi.DesiredJob{
		Image: testImage, Launch: "true",
		Data: testDataAccess(), Token: "test-job-token", GitURL: "https://api.test/git/pe-1.git",
		ID: "data-env", AgentID: "agent", ProjectID: "project",
		AcceleratorType:        "tenstorrent.com/chipArch=blackhole",
		AcceleratorCount:       1,
		EstimatedDurationHours: 0.02, CapacityTier: clusterapi.CapacityGuaranteed,
		Job: clusterapi.JobSpec{
			CPU: "2", Memory: "8Gi", Storage: "5Gi",
			MaxRetries: maxRetriesPtr(0), TerminationGracePeriodSeconds: int64Ptr(5), AcceleratorCount: 1,
			AcceleratorType: "tenstorrent.com/chipArch=blackhole",
		},
	}

	cs, err := e.BuildContainerSpec(exp, Placement{DevicePaths: []string{"/dev/tenstorrent/0"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := cs.Env["HYPOTHESISLOOP_DATA_URI"]; got != exp.Data.URI {
		t.Fatalf("HYPOTHESISLOOP_DATA_URI = %q, want %q (the job's own writable prefix, straight from desired state)", got, exp.Data.URI)
	}
	if got := cs.Env["HYPOTHESISLOOP_DATA_SHARED"]; got != exp.Data.Shared {
		t.Fatalf("HYPOTHESISLOOP_DATA_SHARED = %q, want %q (the platform experiment's readable prefix, straight from desired state)", got, exp.Data.Shared)
	}
	if got := cs.Env["AWS_SECRET_ACCESS_KEY"]; got != exp.Data.SecretAccessKey {
		t.Fatalf("AWS_SECRET_ACCESS_KEY = %q, want %q — an address the job has no credentials for is not an address", got, exp.Data.SecretAccessKey)
	}
	if got := cs.Env["AWS_ACCESS_KEY_ID"]; got != exp.Data.AccessKeyID {
		t.Fatalf("AWS_ACCESS_KEY_ID = %q, want %q — the job's own key, straight from desired state", got, exp.Data.AccessKeyID)
	}
	if _, ok := cs.Env["AWS_SESSION_TOKEN"]; ok {
		t.Fatal("AWS_SESSION_TOKEN is set: a permanent key has no session, and S3 clients reject a key paired with a stray token")
	}
}

// A job's key is permanent and stored with it, so it is desired state like the prefix.
func TestHashContainerSpecDetectsDurableDataKeyDrift(t *testing.T) {
	base := containerSpec{
		Name:  "test-container",
		Image: "example/image:latest",
		Env:   map[string]string{"AWS_ACCESS_KEY_ID": "key", "AWS_SECRET_ACCESS_KEY": "secret"},
	}
	rekeyed := containerSpec{
		Name:  "test-container",
		Image: "example/image:latest",
		Env:   map[string]string{"AWS_ACCESS_KEY_ID": "other-key", "AWS_SECRET_ACCESS_KEY": "other-secret"},
	}
	baseHash, err := hashContainerSpec(base)
	if err != nil {
		t.Fatal(err)
	}
	rekeyedHash, err := hashContainerSpec(rekeyed)
	if err != nil {
		t.Fatal(err)
	}
	if baseHash == rekeyedHash {
		t.Fatal("a different durable-data key left the hash unchanged — a container would keep running with a key that is no longer its own")
	}
}

// The prefix itself is desired state, and a container left writing to a prefix the control plane
// has moved is exactly the drift this hash exists to catch.
func TestHashContainerSpecDetectsDurableDataPrefixDrift(t *testing.T) {
	base := containerSpec{
		Name:  "test-container",
		Image: "example/image:latest",
		Env:   map[string]string{"HYPOTHESISLOOP_DATA_URI": "s3://hl-data/pe-1/agent/exp-1/"},
	}
	moved := containerSpec{
		Name:  "test-container",
		Image: "example/image:latest",
		Env:   map[string]string{"HYPOTHESISLOOP_DATA_URI": "s3://hl-data/pe-1/agent/exp-2/"},
	}

	baseHash, err := hashContainerSpec(base)
	if err != nil {
		t.Fatalf("hashContainerSpec(base): unexpected error: %v", err)
	}
	movedHash, err := hashContainerSpec(moved)
	if err != nil {
		t.Fatalf("hashContainerSpec(moved): unexpected error: %v", err)
	}
	if baseHash == movedHash {
		t.Fatal("hashContainerSpec: hash unchanged when the job's data prefix changed — reconcile will never repoint it")
	}
}

// This runtime executes a job on the one bare node it runs on. Groups are a second way to write
// a multi-node job, so they must meet the same refusal by the same rule — a two-group job that
// slipped through here would run one group's process and silently drop the other, producing a
// learner with no actors that hangs in its rendezvous rather than a rejection anyone can read.
func TestTwoGroupJobIsRejectedByTheSameSingleNodeRuleNumNodesMeets(t *testing.T) {
	e := &Executor{
		apiURL: "http://example.invalid:8083",
	}
	exp := &clusterapi.DesiredJob{
		Image: testImage, Launch: "true",
		Data: testDataAccess(), Token: "test-job-token", GitURL: "https://api.test/git/pe-1.git",
		ID: "grouped", AgentID: "agent", ProjectID: "project",
		EstimatedDurationHours: 0.02, CapacityTier: clusterapi.CapacityGuaranteed,
		Job: clusterapi.JobSpec{
			MaxRetries: maxRetriesPtr(0), TerminationGracePeriodSeconds: int64Ptr(5),
			Groups: []clusterapi.JobGroup{
				{Name: "learner", Replicas: 1, CPU: "2", Memory: "8Gi", Storage: "5Gi"},
				{Name: "actor", Replicas: 3, CPU: "1", Memory: "1Gi", Storage: "1Gi"},
			},
		},
	}

	_, err := e.BuildContainerSpec(exp, Placement{})
	if err == nil {
		t.Fatal("BuildContainerSpec accepted a two-group job on a single-node runtime — it would run one group and drop the rest")
	}
	var unrunnable *agentexec.Unrunnable
	if !errors.As(err, &unrunnable) {
		t.Fatalf("rejection %q is not an Unrunnable, so the job would wait out the stuck-pending timeout with no reason", err)
	}
	if !strings.Contains(err.Error(), "num_nodes=4") || !strings.Contains(err.Error(), "single-node jobs only") {
		t.Fatalf("rejection was %q, want the existing single-node error naming 4 nodes — groups do not make this runtime multi-node, they make its refusal consistent, so they must not invent a second error for the same condition", err)
	}
}

// The other half of that rule: a grouped job that really does total one node is a single-node job
// however it was written, and this runtime must run it — reading its shape and its command off
// the group, since a grouped spec states them nowhere else.
func TestSingleReplicaGroupRunsAndTakesItsShapeFromTheGroup(t *testing.T) {
	e := &Executor{
		apiURL: "http://example.invalid:8083",
	}
	exp := &clusterapi.DesiredJob{
		Image: testImage, Launch: "true",
		Data: testDataAccess(), Token: "test-job-token", GitURL: "https://api.test/git/pe-1.git",
		ID: "one-group", AgentID: "agent", ProjectID: "project",
		AcceleratorType: "tenstorrent.com/chipArch=blackhole", AcceleratorCount: 2,
		EstimatedDurationHours: 0.02, CapacityTier: clusterapi.CapacityGuaranteed,
		Job: clusterapi.JobSpec{
			MaxRetries: maxRetriesPtr(0), TerminationGracePeriodSeconds: int64Ptr(5),
			AcceleratorType: "tenstorrent.com/chipArch=blackhole",
			Groups: []clusterapi.JobGroup{
				{Name: "solo", Replicas: 1, AcceleratorCount: 2, CPU: "2", Memory: "8Gi", Storage: "5Gi",
					Command: []string{"python", "solo.py"}},
			},
		},
	}

	cs, err := e.BuildContainerSpec(exp, Placement{DevicePaths: []string{"/dev/tenstorrent/0", "/dev/tenstorrent/1"}})
	if err != nil {
		t.Fatalf("BuildContainerSpec rejected a one-node grouped job: %v — its node count is 1, which is exactly what this runtime executes", err)
	}
	if got := cs.Env["HYPOTHESISLOOP_ACCELERATOR_COUNT"]; got != "2" {
		t.Fatalf("HYPOTHESISLOOP_ACCELERATOR_COUNT = %q, want 2 — the group's own per-replica count, since a grouped job states it nowhere else", got)
	}
	if len(cs.Command) == 0 || cs.Command[len(cs.Command)-1] != "solo.py" {
		t.Fatalf("command = %v, want the group's own — a grouped job's process is stated per group", cs.Command)
	}
}

func int64Ptr(v int64) *int64 { return &v }

// The window a job declares is recorded on the container it will be stopped with, because the
// only thing DeleteWorkload is given is an experiment id -- the job's declaration is long gone
// from desired state by the time the platform decides to stop it.
func TestContainerRecordsTheDeclaredCheckpointWindow(t *testing.T) {
	e := &Executor{
		apiURL: "http://example.invalid:8083",
	}
	cs, err := e.BuildContainerSpec(checkpointWindowExperiment(int64Ptr(300)), Placement{DevicePaths: []string{"/dev/tenstorrent/0"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := cs.Labels[LabelCheckpointGrace]; got != "300" {
		t.Fatalf("recorded checkpoint window = %q, want %q", got, "300")
	}
}

// A policy-class termination is the platform's own decision about a job that was fine, so the
// container is signalled and then given the window it declared instead of the ordinary grace.
func TestAPolicyTerminationStopsTheContainerOnTheCheckpointWindow(t *testing.T) {
	labels := map[string]string{LabelGraceSeconds: "5", LabelCheckpointGrace: "300"}
	if got, err := stopGrace(labels, true); err != nil || got != 300 {
		t.Fatalf("stop grace = (%d, %v), want 300: a job the platform chose to stop must get its checkpoint window", got, err)
	}
}

// An infrastructure or workload failure gets no window -- there is nothing to save, or nothing
// left to save it with -- and neither does a drift replacement, which is not a termination at
// all and would only be delaying its own recreation.
func TestANonPolicyTerminationStopsTheContainerOnTheOrdinaryGrace(t *testing.T) {
	labels := map[string]string{LabelGraceSeconds: "5", LabelCheckpointGrace: "300"}
	if got, err := stopGrace(labels, false); err != nil || got != 5 {
		t.Fatalf("stop grace = (%d, %v), want 5: only a policy termination may spend the checkpoint window", got, err)
	}
}

// A container with no readable grace is an error, never a default the runtime picks itself.
func TestAContainerWithoutARecordedGraceIsNotStoppedOnAGuess(t *testing.T) {
	if _, err := stopGrace(map[string]string{}, false); err == nil {
		t.Fatal("stopGrace guessed a grace for a container that records none")
	}
}

// Grace is resolved by the control plane at admission; a job without it is refused.
func TestAJobWithoutResolvedGraceIsRefused(t *testing.T) {
	exp := checkpointWindowExperiment(nil)
	exp.Job.TerminationGracePeriodSeconds = nil
	if _, err := (&Executor{apiURL: "http://example.invalid:8083"}).BuildContainerSpec(exp, Placement{DevicePaths: []string{"/dev/tenstorrent/0"}}); err == nil {
		t.Fatal("built a container for a job whose grace admission never resolved")
	}
}

func checkpointWindowExperiment(checkpointGrace *int64) *clusterapi.DesiredJob {
	return &clusterapi.DesiredJob{
		Image: testImage, Launch: "true",
		Data: testDataAccess(), Token: "test-job-token", GitURL: "https://api.test/git/pe-1.git",
		ID: "checkpoint-window", AgentID: "agent", ProjectID: "project",
		AcceleratorType: "tenstorrent.com/chipArch=blackhole", AcceleratorCount: 1,
		EstimatedDurationHours: 0.02, CapacityTier: clusterapi.CapacityGuaranteed,
		Job: clusterapi.JobSpec{
			CPU: "2", Memory: "8Gi", Storage: "5Gi",
			MaxRetries: maxRetriesPtr(0), TerminationGracePeriodSeconds: int64Ptr(5), AcceleratorCount: 1,
			AcceleratorType:        "tenstorrent.com/chipArch=blackhole",
			CheckpointGraceSeconds: checkpointGrace,
		},
	}
}
