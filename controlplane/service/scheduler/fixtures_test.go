package scheduler

import "github.com/scaleresearch/hypothesisloop/controlplane/domain"

const h100 = "nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3"

func distributedExperiment(nodes, acceleratorsPerNode int) *domain.Experiment {
	return &domain.Experiment{
		AcceleratorType: h100,
		Job:             domain.JobSpec{NumNodes: nodes, AcceleratorCount: acceleratorsPerNode, AcceleratorType: h100},
	}
}
