package podexec

import "github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"

// The node offers its whole cores as units of domain.CPUType beside whatever accelerators it has.
// A job on cores is passed no device: its cores are the cpu limit every container already runs
// under, which submission set equal to its count.

func onCores(acceleratorType domain.AcceleratorType) bool {
	return acceleratorType.Kind() == domain.TypeKindCPU
}
