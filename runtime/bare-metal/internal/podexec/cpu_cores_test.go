package podexec

import (
	"reflect"
	"strconv"
	"testing"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
)

const cores = string(clusterapi.CPUType)

// coreJob asks for cores, as submission leaves such a job: its cpu is its count.
func coreJob(id string, count int) *clusterapi.DesiredJob {
	exp := fakeJob(id, "fast", count)
	exp.AcceleratorType, exp.Job.AcceleratorType = clusterapi.CPUType, clusterapi.CPUType
	exp.Job.CPU = strconv.Itoa(count)
	return exp
}

type nodeCapacity struct {
	Available, Total map[string]int64
}

func capacityOf(t *testing.T, e *Executor) nodeCapacity {
	t.Helper()
	available, total, byNode, _, err := e.GetLiveAcceleratorCapacitySnapshot(t.Context())
	if err != nil {
		t.Fatalf("GetLiveAcceleratorCapacitySnapshot: %v", err)
	}
	if !reflect.DeepEqual(byNode, map[string]map[string]int64{e.nodeName: available}) {
		t.Fatalf("by node = %v, want the one node holding %v", byNode, available)
	}
	return nodeCapacity{available, total}
}

func running(f *fakeEngine, ids ...string) func() bool {
	return func() bool {
		for _, id := range ids {
			if f.state(id) != "running" {
				return false
			}
		}
		return true
	}
}

// A node of 32 cores and one chip, the shape of the machine this was built for, offers its cores
// as units of the CPU type with nothing configured. A job on cores and a job on the chip share
// it: the job on cores is handed no device and runs under its cores, and the cores free are those
// neither job holds.
func TestAJobOnCoresAndAChipJobShareTheNode(t *testing.T) {
	previous := totalCPUCores
	totalCPUCores = func() float64 { return 32 }
	t.Cleanup(func() { totalCPUCores = previous })
	f := newFakeEngine()
	e := newFakeExecutor(f, "/dev/fake/0")
	ctx := t.Context()

	idle := nodeCapacity{
		Available: map[string]int64{fakeAcceleratorType: 1, cores: 32},
		Total:     map[string]int64{fakeAcceleratorType: 1, cores: 32},
	}
	if got := capacityOf(t, e); !reflect.DeepEqual(got, idle) {
		t.Fatalf("capacity of the idle node = %+v, want %+v", got, idle)
	}

	onCores, chip := coreJob("cores", 4), fakeJob("chip", "fast", 1)
	chip.Job.CPU = "8"
	pass(t, ctx, e, onCores, chip)
	eventually(t, "both jobs run", running(f, "cores", "chip"))
	eventually(t, "their starts have returned", func() bool { return len(e.operations()) == 0 })
	// A second pass finds both as desired: neither is moved, recreated or reported drifted.
	pass(t, ctx, e, onCores, chip)

	if got := f.devices("cores"); len(got) != 0 {
		t.Errorf("the job on cores was handed %v", got)
	}
	if got := f.devices("chip"); !reflect.DeepEqual(got, []string{"/dev/fake/0"}) {
		t.Errorf("the job on the chip holds %v, want the chip", got)
	}
	if got := f.containers[containerName("cores")].host.NanoCPUs; got != 4_000_000_000 {
		t.Errorf("the job on 4 cores runs under a cpu limit of %d nanocores, want 4 cores", got)
	}
	want := nodeCapacity{
		Available: map[string]int64{fakeAcceleratorType: 0, cores: 20},
		Total:     map[string]int64{fakeAcceleratorType: 1, cores: 32},
	}
	if got := capacityOf(t, e); !reflect.DeepEqual(got, want) {
		t.Fatalf("capacity = %+v, want %+v: 4 cores to the job on cores, the chip and 8 cores to the chip job", got, want)
	}
}
