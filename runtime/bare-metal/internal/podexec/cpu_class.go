package podexec

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/workloadkeys"
)

// A node configured with a CPU class offers its cores as units of the type
// "cpu.hypothesisloop.dev/class=<name>", one unit per whole core, beside whatever accelerators
// it has. A job on the class holds its count of units and is passed no device: its cores are
// the cpu limit every container already runs under, which submission set equal to the count. The
// units a running job holds are read from its container's labels, like an accelerator's.

// ValidCPUClass accepts a name the Kubernetes runtime could carry as a node label value too, so
// one class name means one type on either runtime.
func ValidCPUClass(name string) error {
	if problems := validation.IsValidLabelValue(name); len(problems) > 0 {
		return fmt.Errorf("podexec: CPU class %q is no label value: %s", name, strings.Join(problems, "; "))
	}
	return nil
}

// cpuClassType is the type this node's cores are offered as, or "" on a node with no class.
func (e *Executor) cpuClassType() domain.AcceleratorType {
	if e.cpuClass == "" {
		return ""
	}
	return domain.AcceleratorType(workloadkeys.CPUClassLabel + "=" + e.cpuClass)
}

// onCPUClass reports whether the type is this node's CPU class, whose units are no devices.
func (e *Executor) onCPUClass(acceleratorType domain.AcceleratorType) bool {
	class := e.cpuClassType()
	return class != "" && class.Is(acceleratorType)
}

// cpuClassUnits is the node's whole cores.
func cpuClassUnits() int64 {
	return int64(totalCPUCores())
}
