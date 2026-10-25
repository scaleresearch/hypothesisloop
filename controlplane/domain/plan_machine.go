package domain

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/scaleresearch/hypothesisloop/contract/jobspec"
)

// Machine is what one node of a run's jobs asks for, as its plan states it: the accelerator type
// and the cores, memory and scratch space beside it.
type Machine struct {
	AcceleratorType AcceleratorType `json:"accelerator_type"`
	CPU             string          `json:"cpu"`
	Memory          string          `json:"memory"`
	Storage         string          `json:"storage"`
}

// PlanMachine is the machine a platform experiment's stored proposal states, false for one with
// no proposal or none stated in it.
func PlanMachine(proposal json.RawMessage) (Machine, bool) {
	var m Machine
	if len(proposal) == 0 || json.Unmarshal(proposal, &m) != nil || m.CPU == "" {
		return Machine{}, false
	}
	if m.AcceleratorType == "" {
		m.AcceleratorType = CPUType
	}
	return m, true
}

func (m Machine) String() string {
	return fmt.Sprintf("accelerator_type %s, cpu %s, memory %s, storage %s", m.AcceleratorType, m.CPU, m.Memory, m.Storage)
}

// Differs names what job asks for on each node that is not m, empty when it asks for m. A job
// of groups states its machines per group and is not compared.
func (m Machine) Differs(job JobSpec) string {
	if len(job.Groups) > 0 {
		return ""
	}
	asked := job.AcceleratorType
	if asked == "" || job.AcceleratorCount == 0 && asked.Kind() != TypeKindCPU {
		asked = CPUType
	}
	var differs []string
	if asked != m.AcceleratorType {
		differs = append(differs, fmt.Sprintf("accelerator_type %s", asked))
	}
	for _, q := range []struct{ name, plan, job string }{{"cpu", m.CPU, job.CPU}, {"memory", m.Memory, job.Memory}, {"storage", m.Storage, job.Storage}} {
		if !sameQuantity(q.plan, q.job) {
			differs = append(differs, fmt.Sprintf("%s %s", q.name, q.job))
		}
	}
	return strings.Join(differs, ", ")
}

func sameQuantity(a, b string) bool {
	qa, errA := jobspec.ParseQuantity(a)
	qb, errB := jobspec.ParseQuantity(b)
	if errA != nil || errB != nil {
		return a == b
	}
	return qa.Cmp(qb) == 0
}
