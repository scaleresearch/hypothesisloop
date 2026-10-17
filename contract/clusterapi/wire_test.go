package clusterapi

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func sameJSON(t *testing.T, was, now any) {
	t.Helper()
	wasWire, err := json.Marshal(was)
	if err != nil {
		t.Fatal(err)
	}
	nowWire, err := json.Marshal(now)
	if err != nil {
		t.Fatal(err)
	}
	var wasFields, nowFields any
	if err := json.Unmarshal(wasWire, &wasFields); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(nowWire, &nowFields); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(wasFields, nowFields) {
		t.Fatalf("encodes as %s\nwas %s", nowWire, wasWire)
	}
}

// Before the request had a type, cluster agents sent it as this map. Control planes of either
// age read what agents of either age send.
func TestTheReconcileRequestIsTheOneAgentsSent(t *testing.T) {
	for name, image := range map[string]string{"any image": "", "one image": "registry.test/platform-job:7"} {
		t.Run(name, func(t *testing.T) {
			available := map[string]int64{"nvidia.com/gpu.product=NVIDIA-L40": 1}
			total := map[string]int64{"nvidia.com/gpu.product=NVIDIA-L40": 2}
			byNode := map[string]map[string]int64{"node-a": available}
			resources := map[string]map[string]int64{"node-a": {NodeResourceCPUMillicores: 4000, NodeResourceMemoryBytes: 1 << 30, NodeResourceStorageBytes: 1 << 32}}
			labels := map[string]map[string]string{"node-a": {NodeSlotLabel: "slot-1"}}
			was := map[string]any{
				"cpu_available_cores": 3.5, "cpu_total_cores": 8.0,
				"accelerator_available_by_type": available, "accelerator_total_by_type": total,
				"accelerator_available_by_node": byNode,
				"node_resources_by_node":        resources,
				"node_resources_total_by_node":  resources,
				"node_labels_by_node":           labels,
				"multi_node_capable":            true,
				"job_image":                     image,
				"autoscaler_enabled":            false,
				"builds":                        false,
				"cluster_id":                    "kube-system-uid",
				"ram_available_bytes":           int64(1 << 30), "ram_total_bytes": int64(1 << 31),
				"storage_available_bytes": int64(1 << 32), "storage_total_bytes": int64(1 << 33),
			}
			sameJSON(t, was, ReconcileRequest{
				CPUAvailableCores: 3.5, CPUTotalCores: 8,
				AcceleratorAvailableByType: available, AcceleratorTotalByType: total,
				AcceleratorAvailableByNode: byNode,
				NodeResourcesByNode:        resources,
				NodeResourcesTotalByNode:   resources,
				NodeLabelsByNode:           labels,
				MultiNodeCapable:           true,
				JobImage:                   image,
				ClusterID:                  "kube-system-uid",
				RAMAvailableBytes:          1 << 30, RAMTotalBytes: 1 << 31,
				StorageAvailableBytes: 1 << 32, StorageTotalBytes: 1 << 33,
			})
		})
	}
}

// oldStatusReport and oldJobMachine are the status report as cluster agents declared it before
// it had a type in the contract.
type oldStatusReport struct {
	ExperimentID            string         `json:"experiment_id"`
	Phase                   string         `json:"phase"`
	AdmittedAcceleratorType string         `json:"admitted_accelerator_type,omitempty"`
	AdmittedNode            string         `json:"admitted_node,omitempty"`
	AdmittedMachine         *oldJobMachine `json:"admitted_machine,omitempty"`
	LogTail                 []string       `json:"log_tail,omitempty"`
	Reason                  string         `json:"reason,omitempty"`
	Message                 string         `json:"message,omitempty"`
	RestartCount            int32          `json:"restart_count,omitempty"`
	ScheduledNodes          int32          `json:"scheduled_nodes,omitempty"`
	SchedulingReason        string         `json:"scheduling_reason,omitempty"`
	Attempt                 *int           `json:"attempt,omitempty"`
	StartedAt               *time.Time     `json:"started_at,omitempty"`
	EndedAt                 *time.Time     `json:"ended_at,omitempty"`
}

type oldJobMachine struct {
	InstanceID       string  `json:"instance_id,omitempty"`
	HostID           string  `json:"host_id,omitempty"`
	AcceleratorModel string  `json:"accelerator_model,omitempty"`
	CPUModel         string  `json:"cpu_model,omitempty"`
	PricePerHourUSD  float64 `json:"price_per_hour_usd,omitempty"`
}

func TestTheStatusReportIsTheOneAgentsSent(t *testing.T) {
	attempt := 2
	started := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	ended := started.Add(time.Hour)
	sameJSON(t, map[string]any{"reports": []oldStatusReport{
		{ExperimentID: "job-1", Phase: "gone"},
		{
			ExperimentID: "job-2", Phase: JobPhaseSucceeded.String(), AdmittedAcceleratorType: "nvidia.com/gpu.product=NVIDIA-L40",
			AdmittedNode: "slot-1", AdmittedMachine: &oldJobMachine{InstanceID: "i-1", HostID: "h-1", AcceleratorModel: "L40", CPUModel: "EPYC", PricePerHourUSD: 1.25},
			LogTail: []string{"epoch 1"}, Reason: PhaseReasonContainerFailed, Message: "exit 1", RestartCount: 1, ScheduledNodes: 1,
			SchedulingReason: "scaling up", Attempt: &attempt, StartedAt: &started, EndedAt: &ended,
		},
	}}, StatusPush{Reports: []StatusReport{
		{ExperimentID: "job-1", Phase: "gone"},
		{
			ExperimentID: "job-2", Phase: JobPhaseSucceeded.String(), AdmittedAcceleratorType: "nvidia.com/gpu.product=NVIDIA-L40",
			AdmittedNode: "slot-1", AdmittedMachine: &JobMachine{InstanceID: "i-1", HostID: "h-1", AcceleratorModel: "L40", CPUModel: "EPYC", PricePerHourUSD: 1.25},
			LogTail: []string{"epoch 1"}, Reason: PhaseReasonContainerFailed, Message: "exit 1", RestartCount: 1, ScheduledNodes: 1,
			SchedulingReason: "scaling up", Attempt: &attempt, StartedAt: &started, EndedAt: &ended,
		},
	}})
}
