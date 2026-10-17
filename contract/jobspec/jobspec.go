// Package jobspec holds the rules of what a job states that hl checks before a submit and the
// platform checks at it.
package jobspec

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/resource"
)

// ParseQuantity reads a quantity a job states. It is resource.ParseQuantity with its holes
// closed: a bare sign parses there as 0, which would admit a truncated field as a job asking for
// nothing, and a negative parses cleanly and slips under every cap, which are upper bounds.
func ParseQuantity(qty string) (resource.Quantity, error) {
	if qty == "-" || qty == "+" {
		return resource.Quantity{}, fmt.Errorf("quantity %q has a sign but no number", qty)
	}
	q, err := resource.ParseQuantity(qty)
	if err != nil {
		return resource.Quantity{}, err
	}
	if q.Sign() < 0 {
		return resource.Quantity{}, fmt.Errorf("quantity %q is negative", qty)
	}
	return q, nil
}

// CPUCores reads a cpu quantity, such as "4" or "500m", as cores.
func CPUCores(qty string) (float64, error) {
	q, err := ParseQuantity(qty)
	if err != nil {
		return 0, err
	}
	return float64(q.MilliValue()) / 1000, nil
}

// GB reads a memory or storage quantity, such as "16Gi", as decimal gigabytes (1e9 bytes), the
// unit a GB-hour is billed in.
func GB(qty string) (float64, error) {
	q, err := ParseQuantity(qty)
	if err != nil {
		return 0, err
	}
	return q.AsApproximateFloat64() / 1e9, nil
}

// ShareOfNode is the retired way to ask for a node's share of a quantity, refused by name so the
// answer says what to state instead.
const ShareOfNode = "max"

// NodeError is what is wrong with the cpu, memory or storage one node of a job states.
type NodeError struct {
	// Field is the field as the job spells it, such as job.cpu, and Quantity the quantity it
	// states: cpu, memory or storage.
	Field, Quantity string
	// Unstated is whether the field is left out or asks for a share of the node, where otherwise
	// it is no quantity.
	Unstated bool
	Message  string
}

func (e *NodeError) Error() string { return e.Message }

// CheckNode is the rule for what one node of a job states, owner naming the node as in job or
// job.groups[0], or empty where the three fields stand alone: cpu, memory and storage each
// stated, as a quantity and never a share of the node. What a job holds is what it states: a
// share of the node it lands on held most of a machine for a job that used little, and nothing
// else fitted beside it.
func CheckNode(owner, cpu, memory, storage string) *NodeError {
	for _, quantity := range []struct{ name, value, meaning, example string }{
		{"cpu", cpu, "the cores one node of the job uses", "cpu: 2"},
		{"memory", memory, "the memory one node of the job uses", "memory: 8Gi"},
		{"storage", storage, "the scratch space one node of the job writes", "storage: 4Gi"},
	} {
		field := quantity.name
		if owner != "" {
			field = owner + "." + field
		}
		switch quantity.value {
		case "":
			return &NodeError{Field: field, Quantity: quantity.name, Unstated: true, Message: fmt.Sprintf("%s is required: state %s, as in %s", field, quantity.meaning, quantity.example)}
		case ShareOfNode:
			return &NodeError{Field: field, Quantity: quantity.name, Unstated: true, Message: fmt.Sprintf("%s is %q: %s is stated as a quantity, as in %s, and never as a share of the node", field, quantity.value, quantity.name, quantity.example)}
		}
		if _, err := ParseQuantity(quantity.value); err != nil {
			return &NodeError{Field: field, Quantity: quantity.name, Message: fmt.Sprintf("%s: %s", field, err)}
		}
	}
	return nil
}
