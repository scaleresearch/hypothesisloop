package domain

// NodeSlotLabel marks a node that holds one job at a time, and names it. A runtime that rents a
// machine per job reports each machine it may rent as such a node. Placement then treats the
// node as full once one job is on it, whatever that job requested, and admission writes the
// label into the admitted job's resolved node selector, so the runtime rents exactly the slot
// the control plane counted the job against and decides no placement of its own.
const NodeSlotLabel = "hypothesisloop.io/slot"
