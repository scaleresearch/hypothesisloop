package workload

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestDeviceClassDriverRequiresDirectDriverSelector(t *testing.T) {
	class := unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"name": "example"},
		"spec": map[string]interface{}{"selectors": []interface{}{map[string]interface{}{
			"cel": map[string]interface{}{"expression": `device.driver == "example.com"`},
		}}},
	}}
	driver, err := deviceClassDriver(class)
	if err != nil || driver != "example.com" {
		t.Fatalf("deviceClassDriver = %q, %v", driver, err)
	}
	class.Object["spec"] = map[string]interface{}{"selectors": []interface{}{map[string]interface{}{
		"cel": map[string]interface{}{"expression": "device.attributes['model'] == 'x'"},
	}}}
	if _, err := deviceClassDriver(class); err == nil {
		t.Fatal("unsupported DeviceClass selector was guessed instead of rejected")
	}
}

func TestDRACapacityUsesResourceSlicesAndAllocatedClaims(t *testing.T) {
	slices := []unstructured.Unstructured{{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"name": "slice-a"},
		"spec": map[string]interface{}{
			"driver":   "tenstorrent.com",
			"nodeName": "worker-a",
			"pool":     map[string]interface{}{"name": "pool-a"},
			"devices": []interface{}{
				map[string]interface{}{"name": "chip-0"},
				map[string]interface{}{"name": "chip-1"},
			},
		},
	}}}
	claims := []unstructured.Unstructured{{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"name": "claim-a", "namespace": "jobs"},
		"status": map[string]interface{}{"allocation": map[string]interface{}{"devices": map[string]interface{}{"results": []interface{}{
			map[string]interface{}{"driver": "tenstorrent.com", "pool": "pool-a", "device": "chip-1"},
		}}}},
	}}}

	available, total, err := draCapacity("tenstorrent.com", slices, claims)
	if err != nil {
		t.Fatal(err)
	}
	if available != 1 || total != 2 {
		t.Fatalf("draCapacity = available %d total %d, want 1/2", available, total)
	}
	_, _, byNode, err := draCapacitySnapshot("tenstorrent.com", slices, claims)
	if err != nil {
		t.Fatal(err)
	}
	if byNode["worker-a"] != 1 {
		t.Fatalf("free DRA capacity on worker-a = %d, want 1", byNode["worker-a"])
	}
}

func TestDRACapacityRejectsAllocationMissingFromActualInventory(t *testing.T) {
	claims := []unstructured.Unstructured{{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"name": "claim-a", "namespace": "jobs"},
		"status": map[string]interface{}{"allocation": map[string]interface{}{"devices": map[string]interface{}{"results": []interface{}{
			map[string]interface{}{"driver": "tenstorrent.com", "pool": "pool-a", "device": "chip-0"},
		}}}},
	}}}

	_, _, err := draCapacity("tenstorrent.com", nil, claims)
	if err == nil || !strings.Contains(err.Error(), "absent from current ResourceSlices") {
		t.Fatalf("draCapacity error = %v, want missing inventory error", err)
	}
}
