package domain

import (
	"fmt"
	"slices"
	"strconv"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
)

// NodeSlotLabel marks a node that holds one job at a time, and names it. A runtime that rents a
// machine per job reports each machine it may rent as such a node.
//
// A job admitted to a cluster of slots is resolved to the whole of one: its cpu, memory and
// storage become the slot's, so everything that counts what a job holds (a node's free capacity,
// a cluster's, the credit for preempting it) counts the slot. A slot takes only a job that asks
// for all of it, accelerators included, so one job fills it and a job without accelerators never
// holds a slot that has them. A slot with no accelerators offers its cores as units of CPUType:
// it takes a job on CPU whose cores fit in it, resolved to all of them, count included.
// Admission writes the slot of each node of the job into its resolved job (JobSpec.Slots), so the
// runtime rents exactly the slots the control plane counted the job against and decides no
// placement of its own.
//
// Labels under NodeLabelReservedPrefix are the platform's: a submitted node selector may not
// name one.
const (
	NodeLabelReservedPrefix    = clusterapi.NodeLabelReservedPrefix
	NodeSlotLabel              = clusterapi.NodeSlotLabel
	NodeSlotAcceleratorsLabel  = clusterapi.NodeSlotAcceleratorsLabel
	NodeSlotNoOfferLabel       = clusterapi.NodeSlotNoOfferLabel
	NodeSlotNetworkLabel       = clusterapi.NodeSlotNetworkLabel
	NodeSlotNoNetworkLabel     = clusterapi.NodeSlotNoNetworkLabel
	NodeSlotNetworkWithinLabel = clusterapi.NodeSlotNetworkWithinLabel
	NodeSlotStartupLabel       = clusterapi.NodeSlotStartupLabel
	NodeImageNeedsLabel        = clusterapi.NodeImageNeedsLabel
)

// ValidateSlotLabels refuses the labels of a slot whose count label (NodeSlotAcceleratorsLabel,
// NodeSlotNetworkLabel) is no count or whose NodeSlotNetworkWithinLabel is no level. The labels of
// a node that is no slot pass.
func ValidateSlotLabels(labels map[string]string) error {
	if labels[NodeSlotLabel] == "" {
		return nil
	}
	for _, key := range []string{NodeSlotAcceleratorsLabel, NodeSlotNetworkLabel} {
		value, set := labels[key]
		if count, err := strconv.Atoi(value); set && (err != nil || count < 0) {
			return fmt.Errorf("slot label %s=%q is no count", key, value)
		}
	}
	if within, set := labels[NodeSlotNetworkWithinLabel]; set && !slices.Contains(clusterapi.NodesLevels, NodesLevel(within)) {
		return fmt.Errorf("slot label %s=%q is no level", NodeSlotNetworkWithinLabel, within)
	}
	return nil
}
