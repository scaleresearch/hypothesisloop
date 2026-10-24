package placement

import (
	"reflect"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

func TestCandidateTypes(t *testing.T) {
	for name, tc := range map[string]struct {
		job  domain.JobSpec
		ran  domain.AcceleratorType
		want []domain.AcceleratorType
	}{
		"no accelerator type":            {domain.JobSpec{}, "", nil},
		"the requested type alone":       {domain.JobSpec{AcceleratorType: h100}, "", []domain.AcceleratorType{h100}},
		"requested first, then accepted": {domain.JobSpec{AcceleratorType: h100, AcceptableAcceleratorTypes: []domain.AcceleratorType{l40}}, "", []domain.AcceleratorType{h100, l40}},
		"overlap collapsed": {domain.JobSpec{AcceleratorType: h100, AcceptableAcceleratorTypes: []domain.AcceleratorType{h100, l40, l40}}, "",
			[]domain.AcceleratorType{h100, l40}},
		"a job that ran keeps the type it ran on": {domain.JobSpec{AcceleratorType: h100, AcceptableAcceleratorTypes: []domain.AcceleratorType{l40}}, l40, []domain.AcceleratorType{l40}},
	} {
		t.Run(name, func(t *testing.T) {
			got := CandidateTypes(&domain.Experiment{Job: tc.job, RanOnAcceleratorType: tc.ran})
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("CandidateTypes = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRequiresDistinctHosts(t *testing.T) {
	spread, stack := true, false
	for name, tc := range map[string]struct {
		job  domain.JobSpec
		want bool
	}{
		"one node":                    {domain.JobSpec{AcceleratorCount: 2}, false},
		"several nodes, no devices":   {domain.JobSpec{NumNodes: 2}, false},
		"several nodes with devices":  {domain.JobSpec{NumNodes: 2, AcceleratorCount: 1}, true},
		"spread stated":               {domain.JobSpec{NumNodes: 2, AcceleratorCount: 1, Topology: &domain.TopologySpec{SpreadAcrossHosts: &spread}}, true},
		"spread turned off":           {domain.JobSpec{NumNodes: 2, AcceleratorCount: 1, Topology: &domain.TopologySpec{SpreadAcrossHosts: &stack}}, false},
		"topology without spread set": {domain.JobSpec{NumNodes: 2, AcceleratorCount: 1, Topology: &domain.TopologySpec{}}, true},
		"grouped devices": {domain.JobSpec{Groups: []domain.JobGroup{
			{Name: "learner", Replicas: 1, AcceleratorCount: 2}, {Name: "actor", Replicas: 1}}}, true},
	} {
		t.Run(name, func(t *testing.T) {
			if got := RequiresDistinctHosts(&domain.Experiment{Job: tc.job}); got != tc.want {
				t.Fatalf("RequiresDistinctHosts = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestReportsEveryDimension(t *testing.T) {
	gpu := acceleratorKey("h100")
	for name, tc := range map[string]struct {
		capacity, footprint domain.Footprint
		want                bool
	}{
		"nothing asked":                   {domain.Footprint{}, domain.Footprint{}, true},
		"present and short still reports": {domain.Footprint{cpuKey: 0, gpu: 0}, domain.Footprint{cpuKey: 1000, gpu: 1}, true},
		"a dimension missing":             {domain.Footprint{cpuKey: 8000}, domain.Footprint{cpuKey: 1000, gpu: 1}, false},
		"a zero ask needs no entry":       {domain.Footprint{cpuKey: 8000}, domain.Footprint{cpuKey: 1000, gpu: 0}, true},
		"nil capacity":                    {nil, domain.Footprint{cpuKey: 1}, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := ReportsEveryDimension(tc.capacity, tc.footprint); got != tc.want {
				t.Fatalf("ReportsEveryDimension = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRoomFor(t *testing.T) {
	gpu := acceleratorKey("h100")
	for name, tc := range map[string]struct {
		free, footprint domain.Footprint
		want            int64
	}{
		"limited by devices":       {domain.Footprint{cpuKey: 64000, gpu: 3}, domain.Footprint{cpuKey: 1000, gpu: 1}, 3},
		"limited by cpu":           {domain.Footprint{cpuKey: 2500, gpu: 8}, domain.Footprint{cpuKey: 1000, gpu: 1}, 2},
		"none fits":                {domain.Footprint{cpuKey: 500}, domain.Footprint{cpuKey: 1000}, 0},
		"a missing dimension":      {domain.Footprint{cpuKey: 8000}, domain.Footprint{cpuKey: 1000, gpu: 1}, 0},
		"nothing asked is no room": {domain.Footprint{cpuKey: 8000}, domain.Footprint{}, 0},
	} {
		t.Run(name, func(t *testing.T) {
			if got := roomFor(tc.free, tc.footprint); got != tc.want {
				t.Fatalf("roomFor = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestFitsWhenIdle(t *testing.T) {
	job := func(cpu string, count int, acceptable ...domain.AcceleratorType) *domain.Experiment {
		exp := holdJob("j", domain.CapacityBurst, h100, count, cpu)
		exp.Job.AcceptableAcceleratorTypes = acceptable
		return exp
	}
	cpuOnly := &domain.Experiment{Job: domain.JobSpec{CPU: "4", Memory: "1Gi", Storage: "1Gi"}}
	gang := func(ranks int, spread bool) *domain.Experiment {
		exp := job("4", 1)
		exp.Job.NumNodes = ranks
		exp.Job.Topology = &domain.TopologySpec{SpreadAcrossHosts: &spread}
		return exp
	}
	node := func(cores int64) map[string]int64 {
		return map[string]int64{domain.NodeResourceCPUMillicores: cores * 1000, domain.NodeResourceMemoryBytes: 64 << 30, domain.NodeResourceStorageBytes: 64 << 30}
	}
	for name, tc := range map[string]struct {
		total domain.Footprint
		nodes map[string]map[string]int64
		exp   *domain.Experiment
		want  bool
	}{
		"total covers it, nodes unread":   {accelerators(map[string]int64{h100: 2}), nil, job("4", 2), true},
		"too few devices installed":       {accelerators(map[string]int64{h100: 1}), nil, job("4", 2), false},
		"an accepted type is installed":   {accelerators(map[string]int64{l40: 2}), nil, job("4", 2, l40), true},
		"total covers it, no node does":   {accelerators(map[string]int64{h100: 2}), map[string]map[string]int64{"a": node(8), "b": node(8)}, job("12", 2), false},
		"one node holds it":               {accelerators(map[string]int64{h100: 2}), map[string]map[string]int64{"a": node(8), "b": node(16)}, job("12", 2), true},
		"a job without devices is judged": {accelerators(nil), nil, cpuOnly, true},
		"a job without devices too large": {domain.CapacityFootprint(2, nil, 64<<30, 64<<30), nil, cpuOnly, false},
		"a spread gang, a host each":      {accelerators(map[string]int64{h100: 3}), map[string]map[string]int64{"a": node(8), "b": node(8), "c": node(8)}, gang(3, true), true},
		"a spread gang, too few hosts":    {accelerators(map[string]int64{h100: 3}), map[string]map[string]int64{"a": node(16), "b": node(16)}, gang(3, true), false},
		"a stacked gang, few hosts":       {accelerators(map[string]int64{h100: 3}), map[string]map[string]int64{"a": node(16), "b": node(16)}, gang(3, false), true},
		"a spread gang, a host too small": {accelerators(map[string]int64{h100: 3}), map[string]map[string]int64{"a": node(8), "b": node(8), "c": node(2)}, gang(3, true), false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := FitsWhenIdle(tc.total, tc.nodes, tc.exp); got != tc.want {
				t.Fatalf("FitsWhenIdle = %v, want %v", got, tc.want)
			}
		})
	}
}

// A host taken by a node that fits elsewhere is handed back: the learner fits on "big" alone,
// and an actor placed there first must move to "small" for both to land.
func TestSpreadHostsWhenIdleMovesANodeToFreeTheHostAnotherNeeds(t *testing.T) {
	exp := groupedExperiment(
		domain.JobGroup{Name: "actor", Replicas: 1, CPU: "2", Memory: "1Gi", Storage: "1Gi", AcceleratorCount: 1},
		domain.JobGroup{Name: "learner", Replicas: 1, CPU: "12", Memory: "1Gi", Storage: "1Gi", AcceleratorCount: 1},
	)
	hosts := map[string]map[string]int64{
		"big":   {domain.NodeResourceCPUMillicores: 16000, domain.NodeResourceMemoryBytes: 64 << 30, domain.NodeResourceStorageBytes: 64 << 30},
		"small": {domain.NodeResourceCPUMillicores: 4000, domain.NodeResourceMemoryBytes: 64 << 30, domain.NodeResourceStorageBytes: 64 << 30},
	}
	if got := SpreadHostsWhenIdle(hosts, exp); got != 2 {
		t.Fatalf("SpreadHostsWhenIdle = %d, want 2", got)
	}
}

func TestHardestNodeShape(t *testing.T) {
	exp := groupedExperiment(
		domain.JobGroup{Name: "actor", Replicas: 2, CPU: "8", Memory: "1Gi", Storage: "1Gi"},
		domain.JobGroup{Name: "learner", Replicas: 1, CPU: "1", Memory: "1Gi", Storage: "1Gi", AcceleratorCount: 1},
		domain.JobGroup{Name: "big-learner", Replicas: 1, CPU: "4", Memory: "1Gi", Storage: "1Gi", AcceleratorCount: 1},
	)
	got := HardestNodeShape(exp)
	if got.AcceleratorCount != 1 || got.Resources[domain.NodeResourceCPUMillicores] != 4000 {
		t.Fatalf("HardestNodeShape = %+v, want the learner with a device and the most cpu", got)
	}
	if got := HardestNodeShape(&domain.Experiment{Job: domain.JobSpec{CPU: "2"}}); got.Resources[domain.NodeResourceCPUMillicores] != 2000 {
		t.Fatalf("HardestNodeShape of an ungrouped job = %+v, want its one shape", got)
	}
}

func TestNodeResourceKind(t *testing.T) {
	for resource, want := range map[string]domain.ResourceKind{
		domain.NodeResourceCPUMillicores: domain.ResourceKindCPU,
		domain.NodeResourceMemoryBytes:   domain.ResourceKindMemory,
		domain.NodeResourceStorageBytes:  domain.ResourceKindStorage,
	} {
		if got, ok := NodeResourceKind(resource); !ok || got != want {
			t.Errorf("NodeResourceKind(%q) = %q, %v, want %q", resource, got, ok, want)
		}
	}
	if _, ok := NodeResourceKind("nvidia.com/gpu"); ok {
		t.Error("NodeResourceKind of an accelerator answered ok")
	}
}

func TestLabelsMatch(t *testing.T) {
	for name, tc := range map[string]struct {
		actual, required map[string]string
		want             bool
	}{
		"nothing required":        {nil, nil, true},
		"nothing required of any": {map[string]string{"zone": "a"}, nil, true},
		"all present":             {map[string]string{"zone": "a", "pool": "x"}, map[string]string{"zone": "a"}, true},
		"a value differs":         {map[string]string{"zone": "b"}, map[string]string{"zone": "a"}, false},
		"a label missing":         {map[string]string{}, map[string]string{"zone": "a"}, false},
		"empty value required":    {map[string]string{}, map[string]string{"zone": ""}, true},
	} {
		t.Run(name, func(t *testing.T) {
			if got := LabelsMatch(tc.actual, tc.required); got != tc.want {
				t.Fatalf("LabelsMatch = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestTakesWholeSlot(t *testing.T) {
	whole := map[string]int64{domain.NodeResourceCPUMillicores: 8000, domain.NodeResourceMemoryBytes: 32 << 30, domain.NodeResourceStorageBytes: 100 << 30}
	shape := func(devices int64, resources map[string]int64) domain.NodeShape {
		return domain.NodeShape{AcceleratorCount: devices, Resources: resources}
	}
	half := map[string]int64{domain.NodeResourceCPUMillicores: 4000, domain.NodeResourceMemoryBytes: 32 << 30, domain.NodeResourceStorageBytes: 100 << 30}
	for name, tc := range map[string]struct {
		shape        domain.NodeShape
		accelerators map[string]int64
		resources    map[string]int64
		want         bool
	}{
		"exactly the slot":            {shape(1, whole), map[string]int64{"h100": 1}, whole, true},
		"fewer devices than the slot": {shape(1, whole), map[string]int64{"h100": 2}, whole, false},
		"devices of another type":     {shape(1, whole), map[string]int64{"l40": 1}, whole, false},
		"less cpu than the slot":      {shape(1, half), map[string]int64{"h100": 1}, whole, false},
		"a taken slot":                {shape(1, whole), map[string]int64{"h100": 0}, map[string]int64{}, false},
		"no devices on either side":   {shape(0, whole), map[string]int64{}, whole, true},
	} {
		t.Run(name, func(t *testing.T) {
			if got := takesWholeSlot(tc.shape, "h100", tc.accelerators, tc.resources); got != tc.want {
				t.Fatalf("takesWholeSlot = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestQuantityRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		key, qty string
		value    float64
		canon    string
	}{
		{domain.NodeResourceCPUMillicores, "2", 2000, "2"},
		{domain.NodeResourceCPUMillicores, "500m", 500, "500m"},
		{domain.NodeResourceCPUMillicores, "1.5", 1500, "1500m"},
		{domain.NodeResourceMemoryBytes, "1Gi", 1 << 30, "1Gi"},
		{domain.NodeResourceMemoryBytes, "512Mi", 512 << 20, "512Mi"},
		{domain.NodeResourceStorageBytes, "1G", 1e9, "1000000000"},
	} {
		value, err := QuantityValue(tc.key, tc.qty)
		if err != nil || value != tc.value {
			t.Errorf("QuantityValue(%s, %q) = %v, %v, want %v", tc.key, tc.qty, value, err, tc.value)
		}
		if got := QuantityString(tc.key, value); got != tc.canon {
			t.Errorf("QuantityString(%s, %v) = %q, want %q", tc.key, value, got, tc.canon)
		}
	}
	if _, err := QuantityValue(domain.NodeResourceMemoryBytes, "lots"); err == nil {
		t.Error("QuantityValue accepted a quantity that is not one")
	}
}

func TestClonesAreIndependent(t *testing.T) {
	avail := map[string]domain.Footprint{"c": {cpuKey: 1000}}
	cloned := CloneAvail(avail)
	SubtractFootprint(cloned["c"], domain.Footprint{cpuKey: 400})
	if avail["c"][cpuKey] != 1000 || cloned["c"][cpuKey] != 600 {
		t.Fatalf("CloneAvail shares its footprints: original %v, clone %v", avail, cloned)
	}
	nodes := map[string]map[string]map[string]int64{"c": {"n": {"h100": 2}}}
	clonedNodes := CloneNodesByCluster(nodes)
	clonedNodes["c"]["n"]["h100"] = 0
	if nodes["c"]["n"]["h100"] != 2 {
		t.Fatalf("CloneNodesByCluster shares its node maps: %v", nodes)
	}
}

func TestWithinSharesFootprints(t *testing.T) {
	avail := map[string]domain.Footprint{"a": {cpuKey: 1000}, "b": {cpuKey: 1000}}
	view := within(avail, []string{"a"})
	if len(view) != 1 {
		t.Fatalf("within = %v, want cluster a alone", view)
	}
	SubtractFootprint(view["a"], domain.Footprint{cpuKey: 400})
	if avail["a"][cpuKey] != 600 {
		t.Fatalf("a reservation through the tier view is not seen through avail: %v", avail)
	}
}

func TestSlotNetwork(t *testing.T) {
	for name, tc := range map[string]struct {
		labels map[string]string
		want   int
	}{
		"no label":            {map[string]string{domain.NodeSlotLabel: "a"}, 0},
		"a count":             {map[string]string{domain.NodeSlotNetworkLabel: "4"}, 4},
		"none, with why":      {map[string]string{domain.NodeSlotNetworkLabel: "0", domain.NodeSlotNoNetworkLabel: "no cluster"}, 0},
		"an unreadable label": {map[string]string{domain.NodeSlotNetworkLabel: "many"}, 0},
	} {
		t.Run(name, func(t *testing.T) {
			if got := SlotNetwork(tc.labels); got != tc.want {
				t.Fatalf("SlotNetwork = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestSameShape(t *testing.T) {
	small := map[string]int64{domain.NodeResourceCPUMillicores: 4000}
	large := map[string]int64{domain.NodeResourceCPUMillicores: 8000}
	shapes := []domain.NodeShape{
		{Group: "actor", Resources: small}, {Group: "actor", Resources: small}, {Group: "critic", Resources: small},
		{Group: "learner", AcceleratorCount: 1, Resources: small}, {Group: "big", Resources: large},
	}
	for i, want := range []int{3, 3, 3, 1, 1} {
		if got := sameShape(shapes, shapes[i]); got != want {
			t.Errorf("sameShape of %s = %d, want %d", shapes[i].Group, got, want)
		}
	}
}
