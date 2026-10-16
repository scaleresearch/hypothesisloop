package domain

// NodeSlotLabel marks a node that holds one job at a time, and names it. A runtime that rents a
// machine per job reports each machine it may rent as such a node.
//
// A job admitted to a cluster of slots is resolved to the whole of one: its cpu, memory and
// storage become the slot's, so everything that counts what a job holds (a node's free capacity,
// a cluster's, the credit for preempting it) counts the slot. A slot takes only a job that asks
// for all of it, accelerators included, so one job fills it and a job without accelerators never
// holds a slot that has them. A slot with no accelerators offers its cores as units of CPUType:
// it takes a job on CPU whose cores fit in it, resolved to all of them, count included.
// Admission writes the label into the admitted job's resolved node selector, so the runtime
// rents exactly the slot the control plane counted the job against and decides no placement of
// its own.
//
// Labels under NodeLabelReservedPrefix are the platform's: a submitted node selector may not
// name one.
const (
	NodeLabelReservedPrefix = "hypothesisloop.io/"
	NodeSlotLabel           = NodeLabelReservedPrefix + "slot"
	// NodeSlotAcceleratorsLabel is how many accelerators a slot has, taken or free. Free capacity
	// reads zero for a taken slot, and a job waiting for one still has to be sized against it.
	NodeSlotAcceleratorsLabel = NodeLabelReservedPrefix + "slot-accelerators"
	// NodeSlotNoOfferLabel is on a slot no instance holds and no machine can be rented for right
	// now: the runtime's vendor offers none within the cluster's filters. Such a slot reports
	// nothing free, and nothing is leaving it. Its value is the runtime's reason, in words a
	// person reads ("shape rtx4090: no vast offer of its size passes regions"), which a job that
	// waits for the slot and the cluster's capacity show.
	NodeSlotNoOfferLabel = NodeLabelReservedPrefix + "slot-no-offer"
)
