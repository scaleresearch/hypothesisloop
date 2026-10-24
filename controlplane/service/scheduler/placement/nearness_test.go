package placement

import (
	"maps"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

func jobWithin(level domain.NodesLevel) *domain.Experiment {
	exp := distributedExperiment(2, 1)
	exp.Job.Topology = &domain.TopologySpec{NodesWithin: level}
	return exp
}

func fourNodes() map[string]map[string]int64 {
	return map[string]map[string]int64{"a1": {h100: 1}, "a2": {h100: 1}, "b1": {h100: 1}, "b2": {h100: 1}}
}

// claim is DesiredFits of exp alone, answering the resolved job it writes.
func claim(t *testing.T, byNode map[string]map[string]int64, labels map[string]map[string]string, exp *domain.Experiment) (domain.JobSpec, bool) {
	t.Helper()
	resolved := exp.Job
	return resolved, DesiredFits(byNode, nil, labels, nil, exp, &resolved)
}

func TestAJobOverSeveralNodesIsWithinOneZoneByDefault(t *testing.T) {
	exp := distributedExperiment(2, 1)
	if got := exp.Job.NodesWithin(); got != domain.NodesWithinZone {
		t.Fatalf("NodesWithin = %q, want zone", got)
	}
	exp.Job.NumNodes = 1
	if got := exp.Job.NodesWithin(); got != "" {
		t.Fatalf("a job on one node is within %q, want nothing", got)
	}
}

func TestAJobWithinAZoneTakesNodesOfOneZoneAndNamesIt(t *testing.T) {
	byNode := fourNodes()
	labels := map[string]map[string]string{
		"a1": {domain.NodeZoneLabel: "us-1a"}, "a2": {domain.NodeZoneLabel: "us-1a"},
		"b1": {domain.NodeZoneLabel: "us-1b"}, "b2": {domain.NodeZoneLabel: "us-1b"},
	}
	byNode["a2"][h100] = 0
	resolved, ok := claim(t, byNode, labels, jobWithin(domain.NodesWithinZone))
	if !ok {
		t.Fatal("zone us-1b has two free nodes and the job was refused")
	}
	if want := map[string]string{domain.NodeZoneLabel: "us-1b"}; !maps.Equal(resolved.NodeSelector, want) {
		t.Fatalf("NodeSelector = %v, want %v", resolved.NodeSelector, want)
	}
	byNode["b2"][h100] = 0
	if _, ok := claim(t, byNode, labels, jobWithin(domain.NodesWithinZone)); ok {
		t.Fatal("no zone has two free nodes and the job was placed across zones")
	}
	if !FitsOnlyApart(byNode, nil, labels, jobWithin(domain.NodesWithinZone)) {
		t.Fatal("FitsOnlyApart is false for a job the free nodes hold only across zones")
	}
	if !TopologyFits(byNode, nil, labels, jobWithin(domain.NodesWithinRegion)) {
		t.Fatal("a job within a region was refused nodes of an unlabelled region")
	}
}

func TestAZoneIsWithinItsRegion(t *testing.T) {
	labels := map[string]map[string]string{
		"a1": {domain.NodeRegionLabel: "us", domain.NodeZoneLabel: "z"}, "a2": {domain.NodeRegionLabel: "us", domain.NodeZoneLabel: "z"},
		"b1": {domain.NodeRegionLabel: "eu", domain.NodeZoneLabel: "z"}, "b2": {domain.NodeRegionLabel: "eu", domain.NodeZoneLabel: "z"},
	}
	byNode := fourNodes()
	byNode["a2"][h100], byNode["b2"][h100] = 0, 0
	if TopologyFits(byNode, nil, labels, jobWithin(domain.NodesWithinZone)) {
		t.Fatal("two zones of one name in two regions were taken as one zone")
	}
	byNode["a2"][h100] = 1
	resolved, ok := claim(t, byNode, labels, jobWithin(domain.NodesWithinZone))
	want := map[string]string{domain.NodeRegionLabel: "us", domain.NodeZoneLabel: "z"}
	if !ok || !maps.Equal(resolved.NodeSelector, want) {
		t.Fatalf("placed %v with NodeSelector %v, want %v", ok, resolved.NodeSelector, want)
	}
}

func TestAClusterWithNoZoneLabelsIsOneZone(t *testing.T) {
	resolved, ok := claim(t, fourNodes(), nil, jobWithin(domain.NodesWithinZone))
	if !ok {
		t.Fatal("a cluster without zone labels refused a job within a zone")
	}
	if len(resolved.NodeSelector) != 0 {
		t.Fatalf("NodeSelector = %v, want none on a cluster that names no zone", resolved.NodeSelector)
	}
}

func TestANodeWithoutAZoneInAClusterThatNamesZonesTakesNoJobWithinOne(t *testing.T) {
	labels := map[string]map[string]string{"a1": {domain.NodeZoneLabel: "us-1a"}, "b1": {domain.NodeZoneLabel: "us-1b"}}
	byNode := fourNodes()
	byNode["a2"][h100], byNode["b2"][h100] = 0, 0
	byNode["c"] = map[string]int64{h100: 1}
	if TopologyFits(byNode, nil, labels, jobWithin(domain.NodesWithinZone)) {
		t.Fatal("a node of no zone was taken as the zone of another")
	}
	exp := jobWithin(domain.NodesWithinZone)
	exp.Job.NumNodes = 1
	if !TopologyFits(map[string]map[string]int64{"c": {h100: 1}}, nil, labels, exp) {
		t.Fatal("a job on one node was held to a zone")
	}
}

func TestAPlacementGroupIsTheLabelItsRuntimeNames(t *testing.T) {
	group := func(name string) map[string]string {
		return map[string]string{domain.NodePlacementGroupKeyLabel: "example.com/fabric", "example.com/fabric": name}
	}
	labels := map[string]map[string]string{"a1": group("f1"), "a2": group("f1"), "b1": group("f2"), "b2": {}}
	resolved, ok := claim(t, fourNodes(), labels, jobWithin(domain.NodesWithinPlacementGroup))
	if want := map[string]string{"example.com/fabric": "f1"}; !ok || !maps.Equal(resolved.NodeSelector, want) {
		t.Fatalf("placed %v with NodeSelector %v, want %v", ok, resolved.NodeSelector, want)
	}
	byNode := fourNodes()
	byNode["a2"][h100] = 0
	if TopologyFits(byNode, nil, labels, jobWithin(domain.NodesWithinPlacementGroup)) {
		t.Fatal("a node in no placement group was taken as one in f2")
	}
	if TopologyFits(fourNodes(), nil, nil, jobWithin(domain.NodesWithinPlacementGroup)) {
		t.Fatal("a cluster that names no placement group took a job within one")
	}
}

func TestSlotsInPlacementGroupsAreWithinEveryLevel(t *testing.T) {
	byNode := map[string]map[string]int64{"s0": {h100: 1}, "s1": {h100: 1}}
	resources := map[string]map[string]int64{"s0": {domain.NodeResourceCPUMillicores: 4000}, "s1": {domain.NodeResourceCPUMillicores: 4000}}
	within := string(domain.NodesWithinPlacementGroup)
	labels := map[string]map[string]string{
		"s0": {domain.NodeSlotLabel: "s0", domain.NodeSlotNetworkLabel: "2", domain.NodeSlotNetworkWithinLabel: within},
		"s1": {domain.NodeSlotLabel: "s1", domain.NodeSlotNetworkLabel: "2", domain.NodeSlotNetworkWithinLabel: within},
	}
	for _, level := range []domain.NodesLevel{domain.NodesWithinRegion, domain.NodesWithinZone, domain.NodesWithinPlacementGroup} {
		exp := jobWithin(level)
		exp.Job.CPU = "4"
		resolved := exp.Job
		if !DesiredFits(byNode, resources, labels, nil, exp, &resolved) {
			t.Fatalf("a job within %s was refused two networked slots", level)
		}
		if len(resolved.Slots) != 2 || len(resolved.NodeSelector) != 0 {
			t.Fatalf("within %s: resolved slots %v and selector %v, want two slots and no selector", level, resolved.Slots, resolved.NodeSelector)
		}
	}
}

func TestAnIdleClusterHoldsAJobOnlyInOnePlace(t *testing.T) {
	exp := jobWithin(domain.NodesWithinZone)
	total := exp.Footprint()
	host := map[string]int64{domain.NodeResourceCPUMillicores: 1000}
	nodes := map[string]map[string]int64{"a1": host, "b1": host}
	apart := map[string]map[string]string{"a1": {domain.NodeZoneLabel: "a"}, "b1": {domain.NodeZoneLabel: "b"}}
	if FitsWhenIdle(total, nodes, apart, exp) || !FitsWhenIdleOnlyApart(total, nodes, apart, exp) {
		t.Fatal("two zones of one node each were taken to hold a job of two nodes in one zone")
	}
	together := map[string]map[string]string{"a1": {domain.NodeZoneLabel: "a"}, "b1": {domain.NodeZoneLabel: "a"}}
	if !FitsWhenIdle(total, nodes, together, exp) || FitsWhenIdleOnlyApart(total, nodes, together, exp) {
		t.Fatal("one zone of two nodes does not hold a job of two nodes")
	}
	if !FitsWhenIdle(total, nodes, apart, jobWithin(domain.NodesWithinRegion)) {
		t.Fatal("a job within a region was refused nodes of an unlabelled region")
	}
}

func TestAScaleUpIsHeldToThePlaceWithMostNodesThatRunTheJob(t *testing.T) {
	zoned := map[string]map[string]string{
		"a1": {domain.NodeZoneLabel: "a"},
		"b1": {domain.NodeZoneLabel: "b", "pool": "gpu"},
		"b2": {domain.NodeZoneLabel: "b"},
		"b3": {domain.NodeZoneLabel: "b"},
		"a2": {domain.NodeZoneLabel: "a", "pool": "tpu"},
	}
	for _, tc := range []struct {
		name      string
		labels    map[string]map[string]string
		level     domain.NodesLevel
		selector  map[string]string
		wantPlace map[string]string
		wantOK    bool
	}{
		{"no nodes", nil, domain.NodesWithinZone, nil, nil, true},
		{"most nodes", zoned, domain.NodesWithinZone, nil, map[string]string{domain.NodeZoneLabel: "b"}, true},
		{"most nodes that run it", zoned, domain.NodesWithinZone, map[string]string{"pool": "tpu"}, map[string]string{domain.NodeZoneLabel: "a"}, true},
		{"first by name on a tie", zoned, domain.NodesWithinZone, map[string]string{"pool": "none"}, map[string]string{domain.NodeZoneLabel: "a"}, true},
		{"unlabelled region", zoned, domain.NodesWithinRegion, nil, map[string]string{}, true},
		{"node without a zone", map[string]map[string]string{"a1": {domain.NodeZoneLabel: "a"}, "x": {}}, domain.NodesWithinZone, nil, map[string]string{domain.NodeZoneLabel: "a"}, true},
		{"no node in a place", map[string]map[string]string{"x": {domain.NodeRegionLabel: "r"}, "y": {domain.NodeZoneLabel: "a"}}, domain.NodesWithinZone, nil, nil, false},
		{"placement group", zoned, domain.NodesWithinPlacementGroup, nil, nil, false},
	} {
		exp := jobWithin(tc.level)
		exp.Job.NodeSelector = tc.selector
		place, ok := ScaleUpPlace(tc.labels, exp)
		if ok != tc.wantOK || !maps.Equal(place, tc.wantPlace) {
			t.Errorf("%s: ScaleUpPlace = %v, %v, want %v, %v", tc.name, place, ok, tc.wantPlace, tc.wantOK)
		}
	}
}

func TestAnIdleClusterOfSlotsHoldsAJobOnlyAsNearAsItsNetworks(t *testing.T) {
	slot := map[string]int64{domain.NodeResourceCPUMillicores: 1000}
	nodes := map[string]map[string]int64{"s0": slot, "s1": slot}
	zone := string(domain.NodesWithinZone)
	labels := map[string]map[string]string{
		"s0": {domain.NodeSlotLabel: "s0", domain.NodeSlotNetworkWithinLabel: zone},
		"s1": {domain.NodeSlotLabel: "s1", domain.NodeSlotNetworkWithinLabel: zone},
	}
	exp := jobWithin(domain.NodesWithinPlacementGroup)
	if FitsWhenIdle(exp.Footprint(), nodes, labels, exp) || !FitsWhenIdleOnlyApart(exp.Footprint(), nodes, labels, exp) {
		t.Fatal("slots whose networks are zones were taken to hold a job within a placement group")
	}
	exp = jobWithin(domain.NodesWithinZone)
	if !FitsWhenIdle(exp.Footprint(), nodes, labels, exp) {
		t.Fatal("slots whose networks are zones were refused a job within a zone")
	}
}
