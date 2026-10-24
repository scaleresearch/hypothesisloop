package k8sexec

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
)

// The control plane places a job's nodes in one zone or placement group from the labels the
// agent reports, and a zone is a label every node already carries. A placement group is the label
// this cluster's agent is told the key of, which it names beside the node's labels.
func TestTheAgentReportsTheZoneAndPlacementGroupOfEachNode(t *testing.T) {
	grouped, plain := totalCapacityTestNode("grouped", 8, 32), totalCapacityTestNode("plain", 8, 32)
	grouped.Labels = map[string]string{clusterapi.NodeZoneLabel: "us-1a", "example.com/fabric": "f1"}
	plain.Labels = map[string]string{clusterapi.NodeZoneLabel: "us-1b"}
	dyn := chartDynamic(t, map[schema.GroupVersionResource]string{
		resourceSliceGVR: "ResourceSliceList", resourceClaimGVR: "ResourceClaimList",
		deviceClassGVR: "DeviceClassList", resourceClaimTemplateGVR: "ResourceClaimTemplateList",
	})
	c := &JobWorkloadClient{apiURL: testAPIURL, kube: chartKube(t, grouped, plain), dyn: dyn, placementGroupLabel: "example.com/fabric"}
	_, _, _, labels, err := c.GetLiveAcceleratorCapacitySnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if labels["grouped"][clusterapi.NodeZoneLabel] != "us-1a" || labels["grouped"][clusterapi.NodePlacementGroupKeyLabel] != "example.com/fabric" || labels["grouped"]["example.com/fabric"] != "f1" {
		t.Errorf("labels of a node in a placement group = %v", labels["grouped"])
	}
	if labels["plain"][clusterapi.NodeZoneLabel] != "us-1b" || labels["plain"][clusterapi.NodePlacementGroupKeyLabel] != "" {
		t.Errorf("labels of a node in no placement group = %v", labels["plain"])
	}
	if grouped.Labels[clusterapi.NodePlacementGroupKeyLabel] != "" {
		t.Error("the report wrote into the node it read")
	}
}

// Which zone a job's ranks share is the control plane's decision, written in the job's node
// selector; the runtime adds no rule of its own about zones.
func TestADistributedJobGetsNoZoneAffinity(t *testing.T) {
	spec := clusterapi.JobSpec{NumNodes: 2, Topology: &clusterapi.TopologySpec{NodesWithin: clusterapi.NodesWithinZone}}
	aff := (&JobWorkloadClient{}).buildAffinity("exp", spec, AcceleratorPlacement{}, true)
	if aff == nil || aff.PodAntiAffinity == nil {
		t.Fatalf("affinity = %+v, want the spread across hosts", aff)
	}
	if aff.PodAffinity != nil {
		t.Fatalf("pod affinity = %+v, want none", aff.PodAffinity)
	}
}
