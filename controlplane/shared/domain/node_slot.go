package domain

// NodeSlotLabel marks a node that holds one job at a time, and names it. A runtime that rents a
// machine per job reports each machine it may rent as such a node.
//
// A job admitted to a cluster of slots is resolved to the whole of one: its cpu, memory and
// storage become the slot's, so everything that counts what a job holds (a node's free capacity,
// a cluster's, the credit for preempting it) counts the slot. A slot takes only a job that asks
// for all of it, accelerators included, so one job fills it and a job without accelerators never
// holds a slot that has them. Admission writes the label into the admitted job's resolved node
// selector, so the runtime rents exactly the slot the control plane counted the job against and
// decides no placement of its own.
//
// Labels under NodeLabelReservedPrefix are the platform's: a submitted node selector may not
// name one.
const (
	NodeLabelReservedPrefix = "hypothesisloop.io/"
	NodeSlotLabel           = NodeLabelReservedPrefix + "slot"
	// NodeSlotAcceleratorsLabel is how many accelerators a slot has, taken or free. Free capacity
	// reads zero for a taken slot, and a job waiting for one still has to be sized against it.
	NodeSlotAcceleratorsLabel = NodeLabelReservedPrefix + "slot-accelerators"
)
