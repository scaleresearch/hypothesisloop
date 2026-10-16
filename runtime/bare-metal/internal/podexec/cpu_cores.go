package podexec

import "github.com/scaleresearch/hypothesisloop/contract/clusterapi"

// The node offers its whole cores as units of clusterapi.CPUType beside whatever accelerators it has.
// A job on cores is passed no device: its cores are the cpu limit every container already runs
// under, which submission set equal to its count.

func onCores(acceleratorType clusterapi.AcceleratorType) bool {
	return acceleratorType.Kind() == clusterapi.TypeKindCPU
}
