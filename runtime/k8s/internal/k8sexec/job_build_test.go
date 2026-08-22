package k8sexec

import (
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestBuildJobAddsConfiguredAcceleratorPodResources(t *testing.T) {
	c := &JobWorkloadClient{apiURL: APIURLDefault}
	exp := &domain.Experiment{
		ID: "hugepage-test", AgentID: "agent", ProjectID: "project",
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
		ID: "host-mount-test", AgentID: "agent", ProjectID: "project",
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
	exp := &domain.Experiment{ID: "hash-test", AgentID: "agent", AcceleratorType: "nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3", AcceleratorCount: 1,
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
		ID: "retry-test", AgentID: "agent", ProjectID: "project",
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
	exp := &domain.Experiment{ID: "placement-test", AgentID: "agent", AcceleratorType: "nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3", AcceleratorCount: 1,
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
