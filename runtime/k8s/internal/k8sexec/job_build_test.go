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
