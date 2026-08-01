package k8sexec

import (
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestBuildJobAddsConfiguredAcceleratorPodResources(t *testing.T) {
	c := &JobWorkloadClient{registryURL: RegistryURLDefault}
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

var nvidiaPlacement = AcceleratorPlacement{
	ResourceName:   "nvidia.com/gpu",
	NodeLabelKey:   "nvidia.com/gpu.product",
	NodeLabelValue: "NVIDIA-H100-80GB-HBM3",
}

func TestDesiredSpecHashChangesWithDesiredJob(t *testing.T) {
	c := &JobWorkloadClient{
		registryURL:                          "http://registry",
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
