package k8sexec

import (
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestBuildJobAddsConfiguredAcceleratorPodResources(t *testing.T) {
	c := &JobWorkloadClient{apiURL: APIURLDefault}
	exp := &domain.Experiment{
		Data: testDataAccess(),
		ID:   "hugepage-test", AgentID: "agent", ProjectID: "project",
		AcceleratorType: "tenstorrent.com/chipArch=blackhole", AcceleratorCount: 1,
		EstimatedDurationHours: 0.02, CapacityTier: domain.CapacityGuaranteed,
		Job: domain.JobSpec{
			Image: "example.invalid/workload", CPU: "2", Memory: "8Gi", Storage: "5Gi", MaxRetries: intPtr(3),
			AcceleratorCount: 1, AcceleratorType: "tenstorrent.com/chipArch=blackhole", AcceleratorPodResources: map[string]string{"hugepages-1Gi": "4Gi"},
			// The operator requirement must not be weakened by a submitted extra resource.
			ExtraResources: map[string]string{"hugepages-1Gi": "1Gi"},
		},
	}

	job, err := c.BuildJob(exp, AcceleratorPlacement{DeviceClassName: "tenstorrent.com"})
	if err != nil {
		t.Fatal(err)
	}
	resources := job.Spec.Template.Spec.Containers[0].Resources
	want := resource.MustParse("4Gi")
	if got := resources.Requests["hugepages-1Gi"]; got.Cmp(want) != 0 {
		t.Fatalf("hugepages request = %s, want %s", got.String(), want.String())
	}
	if got := resources.Limits["hugepages-1Gi"]; got.Cmp(want) != 0 {
		t.Fatalf("hugepages limit = %s, want %s", got.String(), want.String())
	}
}

func TestBuildJobAddsReadOnlyHostPathVolumeForHostMounts(t *testing.T) {
	c := &JobWorkloadClient{apiURL: APIURLDefault}
	exp := &domain.Experiment{
		Data: testDataAccess(),
		ID:   "host-mount-test", AgentID: "agent", ProjectID: "project",
		AcceleratorType: "tenstorrent.com/chipArch=blackhole", AcceleratorCount: 1,
		EstimatedDurationHours: 0.02, CapacityTier: domain.CapacityGuaranteed,
		Job: domain.JobSpec{
			Image: "example.invalid/workload", CPU: "2", Memory: "8Gi", Storage: "5Gi", MaxRetries: intPtr(3),
			AcceleratorCount: 1, AcceleratorType: "tenstorrent.com/chipArch=blackhole",
			HostMounts: map[string]string{"/data/dataset": "/var/lib/hypothesisloop/datasets/FOMO_with_dwi"},
		},
	}

	job, err := c.BuildJob(exp, AcceleratorPlacement{DeviceClassName: "tenstorrent.com"})
	if err != nil {
		t.Fatal(err)
	}
	mounts := job.Spec.Template.Spec.Containers[0].VolumeMounts
	if len(mounts) != 1 || mounts[0].MountPath != "/data/dataset" || !mounts[0].ReadOnly {
		t.Fatalf("VolumeMounts = %+v, want one read-only mount at /data/dataset", mounts)
	}
	vols := job.Spec.Template.Spec.Volumes
	if len(vols) != 1 || vols[0].Name != mounts[0].Name {
		t.Fatalf("Volumes = %+v, want one volume named %q", vols, mounts[0].Name)
	}
	if vols[0].HostPath == nil || vols[0].HostPath.Path != "/var/lib/hypothesisloop/datasets/FOMO_with_dwi" {
		t.Fatalf("HostPath volume = %+v, want Path /var/lib/hypothesisloop/datasets/FOMO_with_dwi", vols[0].HostPath)
	}
	if vols[0].HostPath.Type == nil || *vols[0].HostPath.Type != corev1.HostPathDirectory {
		t.Fatalf("HostPath.Type = %v, want HostPathDirectory (fail fast if not present, not DirectoryOrCreate)", vols[0].HostPath.Type)
	}
}

var nvidiaPlacement = AcceleratorPlacement{
	ResourceName:   "nvidia.com/gpu",
	NodeLabelKey:   "nvidia.com/gpu.product",
	NodeLabelValue: "NVIDIA-H100-80GB-HBM3",
}

func TestDesiredSpecHashChangesWithDesiredJob(t *testing.T) {
	c := &JobWorkloadClient{
		apiURL:                               "http://registry",
		defaultTerminationGracePeriodSeconds: 5, maxTerminationGracePeriodSeconds: 30,
	}
	exp := &domain.Experiment{Data: testDataAccess(), ID: "hash-test", AgentID: "agent", AcceleratorType: "nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3", AcceleratorCount: 1,
		EstimatedDurationHours: 1, CapacityTier: domain.CapacityGuaranteed,
		Job: domain.JobSpec{Image: "image:v1", Env: map[string]string{"Z_LAST": "z", "A_FIRST": "a"}, CPU: "1", Memory: "1Gi", Storage: "1Gi", MaxRetries: intPtr(1), AcceleratorCount: 1, AcceleratorType: "nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3"}}
	first, err := c.BuildJob(exp, nvidiaPlacement)
	if err != nil {
		t.Fatal(err)
	}
	secondIdentical, err := c.BuildJob(exp, nvidiaPlacement)
	if err != nil {
		t.Fatal(err)
	}
	if first.Annotations[DesiredSpecHashAnnotation] != secondIdentical.Annotations[DesiredSpecHashAnnotation] {
		t.Fatal("identical desired state produced different spec hashes")
	}
	env := first.Spec.Template.Spec.Containers[0].Env
	if env[len(env)-2].Name != "A_FIRST" || env[len(env)-1].Name != "Z_LAST" {
		t.Fatalf("submitted environment is not canonicalized: tail = %q, %q", env[len(env)-2].Name, env[len(env)-1].Name)
	}
	exp.Job.Image = "image:v2"
	second, err := c.BuildJob(exp, nvidiaPlacement)
	if err != nil {
		t.Fatal(err)
	}
	if first.Annotations[DesiredSpecHashAnnotation] == second.Annotations[DesiredSpecHashAnnotation] {
		t.Fatal("desired spec hash did not change when the image changed")
	}
}

func intPtr(v int) *int { return &v }

func jobWithRetries(retries int) *domain.Experiment {
	return &domain.Experiment{
		Data: testDataAccess(),
		ID:   "retry-test", AgentID: "agent", ProjectID: "project",
		AcceleratorType: "tenstorrent.com/chipArch=blackhole", AcceleratorCount: 1,
		EstimatedDurationHours: 0.02, CapacityTier: domain.CapacityGuaranteed,
		Job: domain.JobSpec{
			Image: "example.invalid/workload", CPU: "2", Memory: "8Gi", Storage: "5Gi",
			MaxRetries:       intPtr(retries),
			AcceleratorCount: 1, AcceleratorType: "tenstorrent.com/chipArch=blackhole",
		},
	}
}

// Retries live at two layers: BackoffLimit recreates the pod, RestartPolicy=OnFailure has the
// kubelet restart the container in place. max_retries only reaches the first, so a job asking
// for zero retries was still restarted — seen live as restart_count=1 with max_retries=0 on a
// diagnostic chosen to fail once and cheaply.
func TestBuildJobMaxRetriesZeroDisablesInPlaceRestart(t *testing.T) {
	c := &JobWorkloadClient{apiURL: APIURLDefault}
	job, err := c.BuildJob(jobWithRetries(0), AcceleratorPlacement{DeviceClassName: "tenstorrent.com"})
	if err != nil {
		t.Fatal(err)
	}
	if got := job.Spec.Template.Spec.RestartPolicy; got != corev1.RestartPolicyNever {
		t.Errorf("restart policy = %q, want Never so max_retries=0 means exactly one attempt", got)
	}
	if got := *job.Spec.BackoffLimit; got != 0 {
		t.Errorf("backoff limit = %d, want 0", got)
	}
}

// With retries actually requested, in-place restart stays on — it is the cheaper recovery for a
// transient fault, and the caller has said retrying is acceptable.
func TestBuildJobKeepsInPlaceRestartWhenRetriesRequested(t *testing.T) {
	c := &JobWorkloadClient{apiURL: APIURLDefault}
	job, err := c.BuildJob(jobWithRetries(2), AcceleratorPlacement{DeviceClassName: "tenstorrent.com"})
	if err != nil {
		t.Fatal(err)
	}
	if got := job.Spec.Template.Spec.RestartPolicy; got != corev1.RestartPolicyOnFailure {
		t.Errorf("restart policy = %q, want OnFailure when retries are requested", got)
	}
	if got := *job.Spec.BackoffLimit; got != 2 {
		t.Errorf("backoff limit = %d, want 2", got)
	}
}

// Placement is this runtime's translation of desired state into local inventory, not desired
// state itself. Hashing it meant a DeviceClass rename or a driver upgrade re-hashed every running
// job of that type, and reconcile drift-deleted them mid-training.
func TestDesiredSpecHashIgnoresResolvedPlacement(t *testing.T) {
	c := &JobWorkloadClient{
		apiURL:                               "http://registry",
		defaultTerminationGracePeriodSeconds: 5, maxTerminationGracePeriodSeconds: 30,
	}
	exp := &domain.Experiment{Data: testDataAccess(), ID: "placement-test", AgentID: "agent", AcceleratorType: "nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3", AcceleratorCount: 1,
		EstimatedDurationHours: 1, CapacityTier: domain.CapacityGuaranteed,
		Job: domain.JobSpec{Image: "image:v1", CPU: "1", Memory: "1Gi", Storage: "1Gi", MaxRetries: intPtr(1), AcceleratorCount: 1, AcceleratorType: "nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3"}}

	first, err := c.BuildJob(exp, nvidiaPlacement)
	if err != nil {
		t.Fatal(err)
	}
	renamed := nvidiaPlacement
	renamed.ResourceName = "nvidia.com/gpu-v2"
	renamed.NodeLabelKey = "nvidia.com/gpu.product.v2"
	second, err := c.BuildJob(exp, renamed)
	if err != nil {
		t.Fatal(err)
	}
	if first.Annotations[DesiredSpecHashAnnotation] != second.Annotations[DesiredSpecHashAnnotation] {
		t.Fatal("re-resolved placement changed the desired spec hash; reconcile would delete the running job")
	}
	// The built Job still carries the new resolution — only the identity ignores it.
	if _, ok := second.Spec.Template.Spec.Containers[0].Resources.Requests["nvidia.com/gpu-v2"]; !ok {
		t.Fatal("built Job did not use the resolved placement")
	}
}

// A pod holds job.accelerator_count devices, never the job total. Injecting the total told
// every pod of an accelerator_count=4, num_nodes=2 job that it had 8, and anything sizing
// --nproc_per_node from it launched four times the processes it had hardware for. The total
// stays on the experiment, which is what admission and billing read.
func TestBuildJobInjectsPerNodeAcceleratorCountNotTheJobTotal(t *testing.T) {
	c := &JobWorkloadClient{apiURL: APIURLDefault}
	exp := &domain.Experiment{
		Data: testDataAccess(),
		ID:   "per-node-count", AgentID: "agent", ProjectID: "project",
		AcceleratorType: "nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3",
		// The job total: 4 per node across 2 nodes, exactly as submission stores it.
		AcceleratorCount:       8,
		EstimatedDurationHours: 0.02, CapacityTier: domain.CapacityGuaranteed,
		Job: domain.JobSpec{
			Image: "example.invalid/workload", CPU: "2", Memory: "8Gi", Storage: "5Gi", MaxRetries: intPtr(0),
			AcceleratorCount: 4, NumNodes: 2,
			AcceleratorType: "nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3",
		},
	}

	job, err := c.BuildJob(exp, nvidiaPlacement)
	if err != nil {
		t.Fatal(err)
	}
	if got := envValue(job, "HYPOTHESISLOOP_ACCELERATOR_COUNT"); got != "4" {
		t.Fatalf("HYPOTHESISLOOP_ACCELERATOR_COUNT = %q, want %q (per node, not the 8-device job total)", got, "4")
	}
	// The pod's own resource request is the same per-node figure — the env var is only true
	// of its pod if it agrees with what the pod was actually given.
	want := resource.MustParse("4")
	if got := job.Spec.Template.Spec.Containers[0].Resources.Limits["nvidia.com/gpu"]; got.Cmp(want) != 0 {
		t.Fatalf("accelerator limit = %s, want %s", got.String(), want.String())
	}
	// The experiment still carries the total, untouched by the build.
	if exp.AcceleratorCount != 8 {
		t.Fatalf("experiment accelerator count = %d, want 8 — the job total must survive the build", exp.AcceleratorCount)
	}
}

func envValue(job *batchv1.Job, name string) string {
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}

// distributedExperiment is a gang of nodes with a retry budget the runtime must NOT try to spend.
func distributedExperiment(nodes, maxRetries int) *domain.Experiment {
	return &domain.Experiment{
		Data: testDataAccess(),
		ID:   "gang", AgentID: "agent", ProjectID: "project",
		AcceleratorType:        "nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3",
		AcceleratorCount:       nodes,
		EstimatedDurationHours: 0.02, CapacityTier: domain.CapacityGuaranteed,
		Job: domain.JobSpec{
			Image: "example.invalid/workload", CPU: "2", Memory: "8Gi", Storage: "5Gi",
			MaxRetries: intPtr(maxRetries), AcceleratorCount: 1, NumNodes: nodes,
			AcceleratorType: "nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3",
		},
	}
}

// G4: a rank failing stops the gang. BackoffLimitPerIndex did the opposite — rank 3 died, k8s
// restarted that index alone, and ranks 0-2 sat blocked in a collective holding their
// accelerators until the gloo/NCCL timeout while the replacement rejoined a rendezvous the
// others had already left. FailJob on any non-zero exit is what tears the survivors down.
func TestDistributedJobFailsTheWholeGangOnAnyRankFailure(t *testing.T) {
	c := &JobWorkloadClient{apiURL: APIURLDefault}
	job, err := c.BuildJob(distributedExperiment(3, 2), nvidiaPlacement)
	if err != nil {
		t.Fatal(err)
	}

	if job.Spec.BackoffLimitPerIndex != nil {
		t.Fatalf("BackoffLimitPerIndex = %d, want unset — per-index retry strands the surviving ranks",
			*job.Spec.BackoffLimitPerIndex)
	}
	if job.Spec.PodFailurePolicy == nil || len(job.Spec.PodFailurePolicy.Rules) != 1 {
		t.Fatal("distributed job has no single pod failure policy rule")
	}
	rule := job.Spec.PodFailurePolicy.Rules[0]
	if rule.Action != batchv1.PodFailurePolicyActionFailJob {
		t.Errorf("pod failure policy action = %q, want %q", rule.Action, batchv1.PodFailurePolicyActionFailJob)
	}
	// NotIn{0} rather than In{137}: an OOM is one non-zero exit among many, and every one of
	// them has to stop the gang, not just the OOM.
	if rule.OnExitCodes == nil || rule.OnExitCodes.Operator != batchv1.PodFailurePolicyOnExitCodesOpNotIn {
		t.Fatalf("exit-code rule = %+v, want operator NotIn", rule.OnExitCodes)
	}
	if len(rule.OnExitCodes.Values) != 1 || rule.OnExitCodes.Values[0] != 0 {
		t.Errorf("exit-code values = %v, want [0] under NotIn (any non-zero exit, 137 included)", rule.OnExitCodes.Values)
	}
	// FailJob is terminal in Kubernetes, so a non-zero BackoffLimit here would be dead machinery
	// implying a retry authority the runtime does not have. Gang retry is the control plane's.
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Errorf("BackoffLimit = %v, want 0 — the runtime must not claim a retry budget it cannot honour", job.Spec.BackoffLimit)
	}
	if job.Spec.CompletionMode == nil || *job.Spec.CompletionMode != batchv1.IndexedCompletion {
		t.Error("distributed job is not an Indexed Job")
	}
	if job.Spec.SuccessPolicy != nil {
		t.Error("a SuccessPolicy would let a lone rank-0 success declare the gang complete")
	}
}

// Single-node jobs keep the runtime's own retry budget: for a one-pod job BackoffLimit really
// does restart the failed unit, so there is nothing for the control plane to take over.
func TestSingleNodeJobKeepsRuntimeRetryBudget(t *testing.T) {
	c := &JobWorkloadClient{apiURL: APIURLDefault}
	job, err := c.BuildJob(distributedExperiment(1, 2), nvidiaPlacement)
	if err != nil {
		t.Fatal(err)
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 2 {
		t.Errorf("BackoffLimit = %v, want 2 (max_retries) — single-node retry is unchanged", job.Spec.BackoffLimit)
	}
	if job.Spec.PodFailurePolicy != nil {
		t.Error("single-node job gained a pod failure policy it never had")
	}
	if job.Spec.CompletionMode != nil {
		t.Error("single-node job was built as an Indexed Job")
	}
}

// Every job in every test gets the same durable-data access the control plane really sends,
// because BuildJob refuses to build a job without one: a job with nowhere to save is a job whose
// result cannot outlive it, and that must fail at build time rather than at the end of a run.
func testDataAccess() *domain.DataAccess {
	return &domain.DataAccess{
		URI: "s3://hl-data/pe-1/agent/exp-1/", Shared: "s3://hl-data/pe-1/",
		Endpoint: "http://store.invalid:9000", Region: "us-east-1",
		AccessKeyID: "key", SecretAccessKey: "secret", SessionToken: "session-token",
	}
}

// Durable data is the only way one job's output reaches the next one, and a job learns where to
// put it from these two variables alone. They must come from desired state and nothing else: the
// control plane places jobs across clusters by capacity, so anything the cluster contributed to
// the address would make a checkpoint's location depend on where the job happened to land — and
// the eval job reading it back has no way to know where that was.
func TestBuildJobInjectsDurableDataAddressFromDesiredState(t *testing.T) {
	c := &JobWorkloadClient{apiURL: APIURLDefault}
	exp := &domain.Experiment{
		Data: testDataAccess(),
		ID:   "data-env", AgentID: "agent", ProjectID: "project",
		AcceleratorType: "tenstorrent.com/chipArch=blackhole", AcceleratorCount: 1,
		EstimatedDurationHours: 0.02, CapacityTier: domain.CapacityGuaranteed,
		Job: domain.JobSpec{
			Image: "example.invalid/workload", CPU: "2", Memory: "8Gi", Storage: "5Gi", MaxRetries: intPtr(0),
			AcceleratorCount: 1, AcceleratorType: "tenstorrent.com/chipArch=blackhole",
		},
	}

	job, err := c.BuildJob(exp, AcceleratorPlacement{DeviceClassName: "tenstorrent.com"})
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	if got := env["HYPOTHESISLOOP_DATA_URI"]; got != exp.Data.URI {
		t.Fatalf("HYPOTHESISLOOP_DATA_URI = %q, want %q (the job's own writable prefix, straight from desired state)", got, exp.Data.URI)
	}
	if got := env["HYPOTHESISLOOP_DATA_SHARED"]; got != exp.Data.Shared {
		t.Fatalf("HYPOTHESISLOOP_DATA_SHARED = %q, want %q (the platform experiment's readable prefix, straight from desired state)", got, exp.Data.Shared)
	}
	if got := env["AWS_SECRET_ACCESS_KEY"]; got != exp.Data.SecretAccessKey {
		t.Fatalf("AWS_SECRET_ACCESS_KEY = %q, want %q — an address the job has no credentials for is not an address", got, exp.Data.SecretAccessKey)
	}
	if got := env["AWS_SESSION_TOKEN"]; got != exp.Data.SessionToken {
		t.Fatalf("AWS_SESSION_TOKEN = %q, want %q — without the session token the scoped credentials are unusable, and the job falls back to no access at all", got, exp.Data.SessionToken)
	}
}

// The control plane mints a fresh session on every reconcile pass, a few seconds apart. If the
// session reached the desired-spec hash, reconcile would see drift every pass and delete every
// running job mid-training — the same failure resolved placement caused, from a different source.
// Rotating an access grant is not the control plane asking for a different job.
func TestDesiredSpecHashIgnoresRotatingDurableDataCredentials(t *testing.T) {
	c := &JobWorkloadClient{apiURL: APIURLDefault}
	exp := &domain.Experiment{
		Data: testDataAccess(),
		ID:   "cred-rotation", AgentID: "agent", ProjectID: "project",
		AcceleratorType: "tenstorrent.com/chipArch=blackhole", AcceleratorCount: 1,
		EstimatedDurationHours: 0.02, CapacityTier: domain.CapacityGuaranteed,
		Job: domain.JobSpec{
			Image: "example.invalid/workload", CPU: "2", Memory: "8Gi", Storage: "5Gi", MaxRetries: intPtr(0),
			AcceleratorCount: 1, AcceleratorType: "tenstorrent.com/chipArch=blackhole",
		},
	}

	first, err := c.BuildJob(exp, AcceleratorPlacement{DeviceClassName: "tenstorrent.com"})
	if err != nil {
		t.Fatal(err)
	}
	rotated := *exp
	rotated.Data = &domain.DataAccess{
		URI: exp.Data.URI, Shared: exp.Data.Shared, Endpoint: exp.Data.Endpoint, Region: exp.Data.Region,
		AccessKeyID: "next-key", SecretAccessKey: "next-secret", SessionToken: "next-session-token",
	}
	second, err := c.BuildJob(&rotated, AcceleratorPlacement{DeviceClassName: "tenstorrent.com"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Annotations[DesiredSpecHashAnnotation] != second.Annotations[DesiredSpecHashAnnotation] {
		t.Fatal("a rotated durable-data session changed the desired-spec hash — reconcile will drift-delete every running job every pass")
	}
}

// The addressing half is genuine desired state and must stay in the hash: a job repointed at a
// different prefix is a different job, and a reconcile that could not see that would leave it
// writing to the old one forever.
func TestDesiredSpecHashChangesWithDurableDataPrefix(t *testing.T) {
	c := &JobWorkloadClient{apiURL: APIURLDefault}
	exp := &domain.Experiment{
		Data: testDataAccess(),
		ID:   "prefix-change", AgentID: "agent", ProjectID: "project",
		AcceleratorType: "tenstorrent.com/chipArch=blackhole", AcceleratorCount: 1,
		EstimatedDurationHours: 0.02, CapacityTier: domain.CapacityGuaranteed,
		Job: domain.JobSpec{
			Image: "example.invalid/workload", CPU: "2", Memory: "8Gi", Storage: "5Gi", MaxRetries: intPtr(0),
			AcceleratorCount: 1, AcceleratorType: "tenstorrent.com/chipArch=blackhole",
		},
	}

	first, err := c.BuildJob(exp, AcceleratorPlacement{DeviceClassName: "tenstorrent.com"})
	if err != nil {
		t.Fatal(err)
	}
	moved := *exp
	movedData := *exp.Data
	movedData.URI = "s3://hl-data/pe-1/agent/exp-2/"
	moved.Data = &movedData
	second, err := c.BuildJob(&moved, AcceleratorPlacement{DeviceClassName: "tenstorrent.com"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Annotations[DesiredSpecHashAnnotation] == second.Annotations[DesiredSpecHashAnnotation] {
		t.Fatal("desired-spec hash unchanged when the job's data prefix changed — reconcile will never repoint it")
	}
}

// A job with nowhere durable to save cannot hand anything to the stage after it, and that is a
// control-plane misconfiguration, not something a runtime may paper over: silently starting the
// run wastes the whole run to discover it.
func TestBuildJobRefusesAnExperimentWithNoDurableDataAccess(t *testing.T) {
	c := &JobWorkloadClient{apiURL: APIURLDefault}
	exp := &domain.Experiment{
		ID: "no-data", AgentID: "agent", ProjectID: "project",
		AcceleratorType: "tenstorrent.com/chipArch=blackhole", AcceleratorCount: 1,
		EstimatedDurationHours: 0.02, CapacityTier: domain.CapacityGuaranteed,
		Job: domain.JobSpec{
			Image: "example.invalid/workload", CPU: "2", Memory: "8Gi", Storage: "5Gi", MaxRetries: intPtr(0),
			AcceleratorCount: 1, AcceleratorType: "tenstorrent.com/chipArch=blackhole",
		},
	}

	if _, err := c.BuildJob(exp, AcceleratorPlacement{DeviceClassName: "tenstorrent.com"}); err == nil {
		t.Fatal("BuildJob: expected an error for an experiment carrying no durable-data access, got nil")
	}
}

// groupedExperiment is the canonical heterogeneous job these tests reason about: one learner on
// eight accelerators alongside three CPU-only actors, as one experiment.
func groupedExperiment() *domain.Experiment {
	return &domain.Experiment{
		Data: testDataAccess(),
		ID:   "grouped", AgentID: "agent", ProjectID: "project",
		AcceleratorType: "tenstorrent.com/chipArch=blackhole", AcceleratorCount: 8,
		EstimatedDurationHours: 0.02, CapacityTier: domain.CapacityGuaranteed,
		Job: domain.JobSpec{
			Image: "example.invalid/workload", MaxRetries: intPtr(2),
			AcceleratorType: "tenstorrent.com/chipArch=blackhole",
			Groups: []domain.JobGroup{
				{Name: "learner", Replicas: 1, AcceleratorCount: 8, CPU: "16", Memory: "128Gi", Storage: "10Gi",
					Command: []string{"python", "learner.py"}},
				{Name: "actor", Replicas: 3, CPU: "1", Memory: "4Gi", Storage: "1Gi",
					Command: []string{"python", "actor.py"}},
			},
		},
	}
}

// Kubernetes has no single object for "one pod of shape A plus three of shape B", so a grouped
// job compiles to one Indexed Job per group — each sized to its OWN group, never to the job. A
// group Job sized to the whole node count would start three extra learners on hardware the job
// never asked for and never paid for.
func TestGroupedJobCompilesToOneIndexedJobPerGroupSizedToThatGroup(t *testing.T) {
	c := &JobWorkloadClient{apiURL: APIURLDefault}

	jobs, err := c.BuildJobs(groupedExperiment(), AcceleratorPlacement{DeviceClassName: "tenstorrent.com"})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 {
		t.Fatalf("BuildJobs returned %d Jobs for a two-group experiment, want 2 — one per group, since a single Job's pods all share one template", len(jobs))
	}
	if jobs[0].Name != "exp-grouped-learner" || jobs[1].Name != "exp-grouped-actor" {
		t.Fatalf("Job names = %q, %q, want exp-grouped-learner and exp-grouped-actor — the name is the reconciliation identity, so it has to name the group it belongs to", jobs[0].Name, jobs[1].Name)
	}
	if got := *jobs[0].Spec.Completions; got != 1 {
		t.Fatalf("learner Job completions = %d, want 1 (its own replica count, not the job's 4 nodes)", got)
	}
	if got := *jobs[1].Spec.Completions; got != 3 {
		t.Fatalf("actor Job completions = %d, want 3 (its own replica count)", got)
	}
	if got := jobs[0].Spec.Template.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]; got.String() != "16" {
		t.Fatalf("learner CPU request = %s, want 16 — each group's pods take that group's shape", got.String())
	}
	if got := jobs[1].Spec.Template.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]; got.String() != "1" {
		t.Fatalf("actor CPU request = %s, want 1 — an actor sized like the learner is the 520-accelerator bill groups exist to avoid", got.String())
	}
	if got := jobs[1].Spec.Template.Spec.Containers[0].Command; len(got) != 2 || got[1] != "actor.py" {
		t.Fatalf("actor command = %v, want the actor group's own — a group with the learner's command is not an actor", got)
	}
	for _, job := range jobs {
		if job.Spec.CompletionMode == nil || *job.Spec.CompletionMode != batchv1.IndexedCompletion {
			t.Fatalf("Job %s is not Indexed — without a completion index a pod has no identity and no resolvable name to be met at", job.Name)
		}
	}
}

// Every node of every group must resolve every other by name, which is what makes a grouped job
// one gang rather than two jobs that happen to run together. That takes ONE headless Service,
// selecting on experiment id alone: a Service per group, or a selector carrying the group label,
// would give each group its own DNS domain and leave the actors unable to find the learner.
func TestGroupedJobPodsShareOneHeadlessServiceAcrossEveryGroup(t *testing.T) {
	c := &JobWorkloadClient{apiURL: APIURLDefault}

	jobs, err := c.BuildJobs(groupedExperiment(), AcceleratorPlacement{DeviceClassName: "tenstorrent.com"})
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range jobs {
		if got := job.Spec.Template.Spec.Subdomain; got != "exp-grouped" {
			t.Fatalf("Job %s subdomain = %q, want exp-grouped — the subdomain IS the shared Service, and a per-group one splits the rendezvous", job.Name, got)
		}
		if got := job.Spec.Template.Labels["hypothesisloop.io/job-group"]; got != "" {
			t.Fatalf("Job %s pod template carries a group label (%q) — the Service selects this experiment's pods by experiment id, so a group label on the pods would exclude every other group", job.Name, got)
		}
		if got := job.Spec.Template.Labels["hypothesisloop.io/experiment-id"]; got != "grouped" {
			t.Fatalf("Job %s pod experiment-id label = %q, want grouped — it is the only thing the shared Service selects on", job.Name, got)
		}
	}
}

// One rendezvous for the whole job. MASTER_ADDR is node 0 of the FIRST group and every container
// of every group is told the same address — if each group pointed at its own node 0, the groups
// would form separate process groups and a collective spanning them would hang until the backend
// timeout with no error anyone can read.
func TestEveryGroupsPodsAreGivenTheFirstGroupsNodeZeroAsMasterAddr(t *testing.T) {
	c := &JobWorkloadClient{apiURL: APIURLDefault}

	jobs, err := c.BuildJobs(groupedExperiment(), AcceleratorPlacement{DeviceClassName: "tenstorrent.com"})
	if err != nil {
		t.Fatal(err)
	}
	want := "exp-grouped-learner-0.exp-grouped." + HypothesisLoopNamespace + ".svc.cluster.local"
	for _, job := range jobs {
		env := envOf(job)
		if got := env["MASTER_ADDR"]; got != want {
			t.Fatalf("Job %s MASTER_ADDR = %q, want %q — every node of every group meets at the first group's node 0", job.Name, got, want)
		}
		if got := env["HYPOTHESISLOOP_MASTER_ADDR"]; got != want {
			t.Fatalf("Job %s HYPOTHESISLOOP_MASTER_ADDR = %q, want %q", job.Name, got, want)
		}
	}
}

// A pod needs two identities: where it sits in its own group (which shard of the actors am I)
// and where it sits in the whole job (my rank among all 4 nodes). WORLD_SIZE spans every node of
// every group, and the global rank is the group's offset plus the pod's index within the group —
// published as two terms because Kubernetes can hand a pod its completion index but cannot add a
// constant to it. A group whose offset was wrong would put two pods at the same global rank,
// which is a silent hang or a corrupt all-reduce rather than an error.
func TestGroupedJobPublishesBothGroupLocalAndJobGlobalRankFacts(t *testing.T) {
	c := &JobWorkloadClient{apiURL: APIURLDefault}

	jobs, err := c.BuildJobs(groupedExperiment(), AcceleratorPlacement{DeviceClassName: "tenstorrent.com"})
	if err != nil {
		t.Fatal(err)
	}
	learner, actor := envOf(jobs[0]), envOf(jobs[1])
	if learner["HYPOTHESISLOOP_GROUP"] != "learner" || actor["HYPOTHESISLOOP_GROUP"] != "actor" {
		t.Fatalf("HYPOTHESISLOOP_GROUP = %q / %q, want learner / actor — a pod that cannot name its own role cannot pick its own code path", learner["HYPOTHESISLOOP_GROUP"], actor["HYPOTHESISLOOP_GROUP"])
	}
	if learner["HYPOTHESISLOOP_GROUP_REPLICAS"] != "1" || actor["HYPOTHESISLOOP_GROUP_REPLICAS"] != "3" {
		t.Fatalf("HYPOTHESISLOOP_GROUP_REPLICAS = %q / %q, want 1 / 3 — each group's own size, which is how an actor knows how many shards to split over", learner["HYPOTHESISLOOP_GROUP_REPLICAS"], actor["HYPOTHESISLOOP_GROUP_REPLICAS"])
	}
	if learner["HYPOTHESISLOOP_RANK_OFFSET"] != "0" || actor["HYPOTHESISLOOP_RANK_OFFSET"] != "1" {
		t.Fatalf("HYPOTHESISLOOP_RANK_OFFSET = %q / %q, want 0 / 1 — the actors' global ranks start after the single learner, and any other offset collides two nodes on one rank", learner["HYPOTHESISLOOP_RANK_OFFSET"], actor["HYPOTHESISLOOP_RANK_OFFSET"])
	}
	if learner["WORLD_SIZE"] != "4" || actor["WORLD_SIZE"] != "4" {
		t.Fatalf("WORLD_SIZE = %q / %q, want 4 in both — the world is the whole job, not the group; a group-sized world builds a process group missing the other group", learner["WORLD_SIZE"], actor["WORLD_SIZE"])
	}
	for _, job := range jobs {
		ref := envSourceOf(job, "HYPOTHESISLOOP_GROUP_RANK")
		if ref == nil || ref.FieldRef == nil || ref.FieldRef.FieldPath != "metadata.annotations['batch.kubernetes.io/job-completion-index']" {
			t.Fatalf("Job %s HYPOTHESISLOOP_GROUP_RANK is not read from the pod's completion index — a group-local rank baked into the template is the same value in every pod of the group", job.Name)
		}
		if _, present := envOf(job)["RANK"]; present {
			t.Fatalf("Job %s sets RANK, which Kubernetes cannot compute for a grouped job (offset + index is arithmetic, and env expansion only concatenates) — the group-local index published under that name would silently be wrong for every group after the first", job.Name)
		}
	}
}

// A gang is one unit: any pod failing must stop the whole set. Each group Job carries the same
// FailJob-on-any-non-zero-exit policy a distributed Job has carried since gangs were fixed, and
// BackoffLimit stays pinned to 0 so the control plane's retry remains the single retry authority —
// a group Job left with a positive BackoffLimit would restart its own index while the other
// group's pods sat blocked in a collective, burning the whole allocation to fail anyway.
func TestAFailureInEitherGroupIsSetToFailThatGroupsJobOutright(t *testing.T) {
	c := &JobWorkloadClient{apiURL: APIURLDefault}

	jobs, err := c.BuildJobs(groupedExperiment(), AcceleratorPlacement{DeviceClassName: "tenstorrent.com"})
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range jobs {
		if job.Spec.PodFailurePolicy == nil || len(job.Spec.PodFailurePolicy.Rules) != 1 {
			t.Fatalf("Job %s has no pod failure policy — a pod of one group could die while the other group kept running and holding accelerators", job.Name)
		}
		rule := job.Spec.PodFailurePolicy.Rules[0]
		if rule.Action != batchv1.PodFailurePolicyActionFailJob || rule.OnExitCodes == nil ||
			rule.OnExitCodes.Operator != batchv1.PodFailurePolicyOnExitCodesOpNotIn || len(rule.OnExitCodes.Values) != 1 || rule.OnExitCodes.Values[0] != 0 {
			t.Fatalf("Job %s pod failure policy is %+v, want FailJob on any non-zero exit — a gang succeeds or fails as one", job.Name, rule)
		}
		if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
			t.Fatalf("Job %s BackoffLimit is not 0 — Kubernetes cannot restart a gang, so max_retries is the control plane's decision and two retry authorities would disagree", job.Name)
		}
		if job.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyNever {
			t.Fatalf("Job %s restart policy = %q, want Never — the failure policy acts on failed pods, and a container restarted in place never becomes one", job.Name, job.Spec.Template.Spec.RestartPolicy)
		}
	}
}

// The backward-compatibility claim for this backend, asserted rather than assumed: adding groups
// must not have moved anything about a job that declares none. Same single Job, same name, same
// per-node shape, same rank vars, same Service — because the ungrouped path now runs through the
// same group machinery, and any divergence would rewrite every job already running in a cluster.
func TestAnUngroupedJobStillCompilesToExactlyTheJobItAlwaysDid(t *testing.T) {
	c := &JobWorkloadClient{apiURL: APIURLDefault}
	exp := &domain.Experiment{
		Data: testDataAccess(),
		ID:   "ungrouped", AgentID: "agent", ProjectID: "project",
		AcceleratorType: "tenstorrent.com/chipArch=blackhole", AcceleratorCount: 4,
		EstimatedDurationHours: 0.02, CapacityTier: domain.CapacityGuaranteed,
		Job: domain.JobSpec{
			Image: "example.invalid/workload", CPU: "2", Memory: "8Gi", Storage: "5Gi", MaxRetries: intPtr(2),
			AcceleratorCount: 2, AcceleratorType: "tenstorrent.com/chipArch=blackhole", NumNodes: 2,
			Command: []string{"python", "train.py"},
		},
	}

	jobs, err := c.BuildJobs(exp, AcceleratorPlacement{DeviceClassName: "tenstorrent.com"})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("BuildJobs returned %d Jobs for an ungrouped experiment, want 1 — more than one would orphan the Job already running under the old name", len(jobs))
	}
	job := jobs[0]
	if job.Name != "exp-ungrouped" {
		t.Fatalf("Job name = %q, want exp-ungrouped — the name is the reconciliation identity of every job that already exists", job.Name)
	}
	if job.Labels["hypothesisloop.io/job-group"] != "" {
		t.Fatalf("an ungrouped Job carries a group label — it has no groups, and the label would change its identity for anything that reads it")
	}
	if got := job.Spec.Template.Spec.Subdomain; got != "exp-ungrouped" {
		t.Fatalf("subdomain = %q, want exp-ungrouped", got)
	}
	env := envOf(job)
	want := "exp-ungrouped-0.exp-ungrouped." + HypothesisLoopNamespace + ".svc.cluster.local"
	if env["MASTER_ADDR"] != want {
		t.Fatalf("MASTER_ADDR = %q, want %q — the rendezvous name of an existing distributed job must not move", env["MASTER_ADDR"], want)
	}
	if env["WORLD_SIZE"] != "2" || env["LOCAL_RANK"] != "0" {
		t.Fatalf("WORLD_SIZE/LOCAL_RANK = %q/%q, want 2/0", env["WORLD_SIZE"], env["LOCAL_RANK"])
	}
	if env["HYPOTHESISLOOP_ACCELERATOR_COUNT"] != "2" {
		t.Fatalf("HYPOTHESISLOOP_ACCELERATOR_COUNT = %q, want 2 — the PER-NODE count, never the job's total of 4", env["HYPOTHESISLOOP_ACCELERATOR_COUNT"])
	}
	if _, present := env["HYPOTHESISLOOP_GROUP"]; present {
		t.Fatalf("an ungrouped job was told a group name — it has no groups, and a workload branching on that variable would take the wrong path")
	}
	if ref := envSourceOf(job, "RANK"); ref == nil || ref.FieldRef == nil {
		t.Fatalf("RANK is no longer read from the pod's completion index — an ungrouped distributed job's ranks are exactly its node indexes and every existing workload reads them that way")
	}
	if got := job.Spec.Template.Spec.Containers[0].Command; len(got) != 2 || got[1] != "train.py" {
		t.Fatalf("command = %v, want the job's own — an ungrouped job has no group to take one from", got)
	}
	if got := *job.Spec.Completions; got != 2 {
		t.Fatalf("completions = %d, want 2 (num_nodes)", got)
	}
	if single, err := c.BuildJob(exp, AcceleratorPlacement{DeviceClassName: "tenstorrent.com"}); err != nil || single.Name != job.Name {
		t.Fatalf("BuildJob no longer returns the single compiled Job for an ungrouped experiment (%v) — every existing caller reads exactly one", err)
	}
}

// envOf/envSourceOf read a compiled Job's container environment the way a pod would see it.
func envOf(job *batchv1.Job) map[string]string {
	out := map[string]string{}
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		if e.ValueFrom == nil {
			out[e.Name] = e.Value
		} else {
			out[e.Name] = ""
		}
	}
	return out
}

func envSourceOf(job *batchv1.Job, name string) *corev1.EnvVarSource {
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		if e.Name == name {
			return e.ValueFrom
		}
	}
	return nil
}
