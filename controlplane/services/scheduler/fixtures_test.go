package scheduler

import "github.com/scaleresearch/hypothesisloop/controlplane/domain"

const h100 = "nvidia.com/gpu.product=nvidia-h100-80gb-hbm3"

func distributedExperiment(nodes, acceleratorsPerNode int) *domain.Experiment {
	return &domain.Experiment{
		AcceleratorType: h100,
		Job:             domain.JobSpec{Image: "registry.test/job:1", NumNodes: nodes, AcceleratorCount: acceleratorsPerNode, AcceleratorType: h100},
	}
}
