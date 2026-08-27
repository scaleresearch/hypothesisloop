package k8sexec

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
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

func newDRATestClient(objects ...runtime.Object) *JobWorkloadClient {
	scheme := runtime.NewScheme()
	listKinds := map[schema.GroupVersionResource]string{
		resourceSliceGVR: "ResourceSliceList",
		resourceClaimGVR: "ResourceClaimList",
		deviceClassGVR:   "DeviceClassList",
	}
	return &JobWorkloadClient{dyn: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds, objects...)}
}

func tenstorrentDeviceClass() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "resource.k8s.io/v1", "kind": "DeviceClass",
		"metadata": map[string]interface{}{"name": "tenstorrent"},
		"spec": map[string]interface{}{"selectors": []interface{}{map[string]interface{}{
			"cel": map[string]interface{}{"expression": `device.driver == "tenstorrent.com"`},
		}}},
	}}
}

func tenstorrentResourceSlice(name, node string, devices ...string) *unstructured.Unstructured {
	items := make([]interface{}, 0, len(devices))
	for _, d := range devices {
		items = append(items, map[string]interface{}{
			"name": d,
			"attributes": map[string]interface{}{
				"chipArch": map[string]interface{}{"stringValue": "blackhole"},
			},
		})
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "resource.k8s.io/v1", "kind": "ResourceSlice",
		"metadata": map[string]interface{}{"name": name},
		"spec": map[string]interface{}{
			"driver":   "tenstorrent.com",
			"nodeName": node,
			"pool":     map[string]interface{}{"name": "pool-a"},
			"devices":  items,
		},
	}}
}

// TestLiveDRACapacitySnapshotsRejectsTransientEmptyListing is the regression test for the
// production incident this fix closes: a configured driver (tenstorrent.com, via its
// DeviceClass) that returns zero matching ResourceSlices in one List call must not be reported
// as "this cluster has no such hardware" — that silently zeroed a live 3/4-free blackhole
// snapshot and made the scheduler deny burst admission with a false capacity_unavailable while
// real chips sat free. See fix-later.md for the live incident and controlplane/services/
// scheduler/loop_tick.go's reportsEveryDimension for the scheduler side that consumed the bad
// snapshot.
func TestLiveDRACapacitySnapshotsRejectsTransientEmptyListing(t *testing.T) {
	c := newDRATestClient(tenstorrentDeviceClass())

	_, err := c.liveDRACapacitySnapshots(context.Background(), map[string]bool{"worker-a": true})
	if err == nil {
		t.Fatal("liveDRACapacitySnapshots silently reported zero capacity for a configured driver with no ResourceSlices; want an error")
	}
	if !strings.Contains(err.Error(), "tenstorrent.com") || !strings.Contains(err.Error(), "no ResourceSlices") {
		t.Fatalf("liveDRACapacitySnapshots error = %v, want it to name the driver and the missing-listing condition", err)
	}
}

// TestLiveDRACapacitySnapshotsReportsRealInventory is the control case: a driver whose
// ResourceSlices ARE present must still report full capacity, including a flavor that is
// currently fully allocated (available 0, total > 0) — proving the fix above doesn't regress
// the ordinary "hardware is just busy" case into the same error.
func TestLiveDRACapacitySnapshotsReportsRealInventory(t *testing.T) {
	c := newDRATestClient(
		tenstorrentDeviceClass(),
		tenstorrentResourceSlice("slice-a", "worker-a", "chip-0", "chip-1", "chip-2", "chip-3"),
	)

	out, err := c.liveDRACapacitySnapshots(context.Background(), map[string]bool{"worker-a": true})
	if err != nil {
		t.Fatalf("liveDRACapacitySnapshots: %v", err)
	}
	entry, ok := out["tenstorrent.com/chipArch=blackhole"]
	if !ok {
		t.Fatalf("liveDRACapacitySnapshots = %v, missing tenstorrent.com/chipArch=blackhole", out)
	}
	if entry.available != 4 || entry.total != 4 {
		t.Fatalf("entry = %+v, want available=4 total=4", entry)
	}
}

// TestLiveDRACapacitySnapshotsUnrelatedDriverUnaffected proves the fix is scoped to the
// driver that actually went missing: a second configured driver with real inventory still
// reports normally even while another driver's listing came back empty.
func TestLiveDRACapacitySnapshotsUnrelatedDriverUnaffected(t *testing.T) {
	otherClass := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "resource.k8s.io/v1", "kind": "DeviceClass",
		"metadata": map[string]interface{}{"name": "other"},
		"spec": map[string]interface{}{"selectors": []interface{}{map[string]interface{}{
			"cel": map[string]interface{}{"expression": `device.driver == "other.example.com"`},
		}}},
	}}
	otherSlice := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "resource.k8s.io/v1", "kind": "ResourceSlice",
		"metadata": map[string]interface{}{"name": "slice-other"},
		"spec": map[string]interface{}{
			"driver": "other.example.com", "nodeName": "worker-a",
			"pool":    map[string]interface{}{"name": "pool-other"},
			"devices": []interface{}{map[string]interface{}{"name": "dev-0"}},
		},
	}}
	c := newDRATestClient(tenstorrentDeviceClass(), otherClass, otherSlice)

	_, err := c.liveDRACapacitySnapshots(context.Background(), map[string]bool{"worker-a": true})
	if err == nil || !strings.Contains(err.Error(), "tenstorrent.com") {
		t.Fatalf("liveDRACapacitySnapshots error = %v, want it to name the specific driver with no ResourceSlices (tenstorrent.com)", err)
	}
}

// TestLiveDRACapacitySnapshotsRejectsMissingDeviceClass is the regression test for the second,
// symmetric half of the same bug class: a DeviceClasses listing that transiently comes back
// empty (or just missing the installed driver's entry) leaves `drivers` without that domain, so
// the per-driver loops iterate zero times for it and — before this fix — silently returned zero
// capacity for real, currently-installed hardware, with no error at all (the sawSliceForDriver
// check only guards drivers DeviceClasses DID discover). Live incident: pe-e11aa080,
// smri11-heterofrofastacked-local-v2 stuck QUEUED ~2026-08-27T17:59-18:05Z with
// cluster_unresolved=true despite GreptimeDB and kubectl both confirming 3/4 blackhole chips free
// and no error logged from the first (ResourceSlices-side) fix — see fix-later.md.
func TestLiveDRACapacitySnapshotsRejectsMissingDeviceClass(t *testing.T) {
	c := newDRATestClient(tenstorrentResourceSlice("slice-a", "worker-a", "chip-0"))

	_, err := c.liveDRACapacitySnapshots(context.Background(), map[string]bool{"worker-a": true})
	if err == nil {
		t.Fatal("liveDRACapacitySnapshots silently reported zero capacity for a driver with real ResourceSlices but no matching DeviceClass; want an error")
	}
	if !strings.Contains(err.Error(), "tenstorrent.com") || !strings.Contains(err.Error(), "no matching DeviceClass") {
		t.Fatalf("liveDRACapacitySnapshots error = %v, want it to name the driver and the missing-DeviceClass condition", err)
	}
}
