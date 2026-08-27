package k8sexec

// Kubernetes Dynamic Resource Allocation support lives here so the general Job lifecycle and
// scheduler remain device-mechanism-neutral. The current production catalog selects DRA for
// Tenstorrent, but this adapter is vendor-independent and uses the configured DeviceClass.

import (
	"context"
	stderrors "errors"
	"fmt"
	"log"
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/workloadkeys"
)

const draClaimName = "accelerator"

var resourceClaimTemplateGVR = schema.GroupVersionResource{
	Group: "resource.k8s.io", Version: "v1", Resource: "resourceclaimtemplates",
}

var resourceSliceGVR = schema.GroupVersionResource{
	Group: "resource.k8s.io", Version: "v1", Resource: "resourceslices",
}

var resourceClaimGVR = schema.GroupVersionResource{
	Group: "resource.k8s.io", Version: "v1", Resource: "resourceclaims",
}

var deviceClassGVR = schema.GroupVersionResource{
	Group: "resource.k8s.io", Version: "v1", Resource: "deviceclasses",
}

func resourceClaimTemplateName(jobName string) string { return jobName + "-accelerator" }

type draLiveCapacity struct {
	available  int64
	total      int64
	freeByNode map[string]int64
}

// liveDRACapacitySnapshots reads standard Kubernetes DRA inventory and allocations once, then
// reports capacity keyed by accelerator type — the driver-published "<driver>/<attribute>=<value>"
// strings a job spec names directly (see domain.AcceleratorType).
//
// Every attribute a device publishes yields one type, so the same physical device is counted
// under each fact that is true of it (vendor=tenstorrent, chipArch=blackhole, boardType=7, ...).
// These are selectors, not a partition: a job requests exactly one of them, and its capacity
// check reads only that key, so the overlap is correct rather than double-counting. It also
// means the platform never has to decide which attribute is "the model name" — the operator
// prices whichever granularity they care about, and that is the one agents use.
// schedulable bounds the inventory to nodes that can actually accept work. DRA slices outlive
// their node's health: a machine that is powered off, whose kubelet died, or that lost its
// network stays registered and keeps advertising its devices, so without this filter its chips
// are reported free forever and jobs are admitted against hardware that no longer exists.
func (c *JobWorkloadClient) liveDRACapacitySnapshots(ctx context.Context, schedulable map[string]bool) (map[string]draLiveCapacity, error) {
	slices, err := c.dyn.Resource(resourceSliceGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("workload: list DRA ResourceSlices: %w", err)
	}
	claims, err := c.dyn.Resource(resourceClaimGVR).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("workload: list DRA ResourceClaims: %w", err)
	}
	classes, err := c.dyn.Resource(deviceClassGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("workload: list DRA DeviceClasses: %w", err)
	}

	drivers := make(map[string]bool, len(classes.Items))
	for _, class := range classes.Items {
		driver, err := deviceClassDriver(class)
		if err != nil {
			return nil, err
		}
		drivers[driver] = true
	}

	allocated, err := allocatedDRADevices(claims.Items)
	if err != nil {
		return nil, err
	}

	out := make(map[string]draLiveCapacity)
	sawSliceForDriver := make(map[string]bool, len(drivers))
	for driver := range drivers {
		for _, slice := range slices.Items {
			sliceDriver, found, err := unstructured.NestedString(slice.Object, "spec", "driver")
			if err != nil || !found {
				return nil, fmt.Errorf("workload: ResourceSlice %q has no valid spec.driver", slice.GetName())
			}
			if sliceDriver != driver {
				continue
			}
			// Recorded before the schedulable filter below: a node going offline legitimately
			// drops that node's devices from capacity, but the driver having reported zero
			// ResourceSlices at all this listing is a different, suspicious condition (a listing
			// race during republish, an API hiccup) that must not be read as "this hardware no
			// longer exists" — see the error below.
			sawSliceForDriver[driver] = true
			node, _, err := unstructured.NestedString(slice.Object, "spec", "nodeName")
			if err != nil {
				return nil, fmt.Errorf("workload: ResourceSlice %q has invalid spec.nodeName", slice.GetName())
			}
			if node != "" && !schedulable[node] {
				continue
			}
			pool, found, err := unstructured.NestedString(slice.Object, "spec", "pool", "name")
			if err != nil || !found || pool == "" {
				return nil, fmt.Errorf("workload: ResourceSlice %q has no valid spec.pool.name", slice.GetName())
			}
			items, found, err := unstructured.NestedSlice(slice.Object, "spec", "devices")
			if err != nil || !found {
				return nil, fmt.Errorf("workload: ResourceSlice %q has no device inventory", slice.GetName())
			}
			for _, item := range items {
				device, ok := item.(map[string]interface{})
				if !ok {
					return nil, fmt.Errorf("workload: ResourceSlice %q contains malformed device", slice.GetName())
				}
				name, ok := device["name"].(string)
				if !ok || name == "" {
					return nil, fmt.Errorf("workload: ResourceSlice %q contains device without name", slice.GetName())
				}
				free := !allocated[pool+"/"+name]
				for attribute, value := range deviceAttributeStrings(device) {
					key := driver + "/" + attribute + "=" + value
					entry, seen := out[key]
					if !seen {
						entry = draLiveCapacity{freeByNode: map[string]int64{}}
					}
					entry.total++
					if free {
						entry.available++
						if node != "" {
							entry.freeByNode[node]++
						}
					}
					out[key] = entry
				}
			}
		}
	}
	for driver := range drivers {
		if !sawSliceForDriver[driver] {
			// A configured DeviceClass with zero matching ResourceSlices this observation is
			// almost certainly a transient listing gap (informer republish, API hiccup), not the
			// driver's hardware actually vanishing — see the field comment above. Reporting empty
			// capacity here would make the caller publish a snapshot with the whole flavor
			// missing, which the scheduler cannot tell apart from "no cluster has this hardware"
			// (see reportsEveryDimension in controlplane/services/scheduler/loop_tick.go) and
			// wrongly denies burst admission with capacity_unavailable despite real free chips.
			return nil, fmt.Errorf("workload: DRA driver %q has a configured DeviceClass but reported no ResourceSlices this observation", driver)
		}
	}
	return out, nil
}

// allocatedDRADevices returns the set of "<pool>/<device>" identities currently held by a claim.
func allocatedDRADevices(claims []unstructured.Unstructured) (map[string]bool, error) {
	allocated := make(map[string]bool)
	for _, claim := range claims {
		results, found, err := unstructured.NestedSlice(claim.Object, "status", "allocation", "devices", "results")
		if err != nil {
			return nil, fmt.Errorf("workload: ResourceClaim %s/%s has malformed allocation: %w", claim.GetNamespace(), claim.GetName(), err)
		}
		if !found {
			continue
		}
		for _, item := range results {
			result, ok := item.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("workload: ResourceClaim %s/%s has malformed allocation result", claim.GetNamespace(), claim.GetName())
			}
			pool, poolOK := result["pool"].(string)
			device, deviceOK := result["device"].(string)
			if !poolOK || !deviceOK || pool == "" || device == "" {
				return nil, fmt.Errorf("workload: ResourceClaim %s/%s has incomplete allocation identity", claim.GetNamespace(), claim.GetName())
			}
			allocated[pool+"/"+device] = true
		}
	}
	return allocated, nil
}

func deviceClassDriver(class unstructured.Unstructured) (string, error) {
	selectors, found, err := unstructured.NestedSlice(class.Object, "spec", "selectors")
	if err != nil || !found || len(selectors) != 1 {
		return "", fmt.Errorf("workload: DeviceClass %q must have exactly one CEL driver selector", class.GetName())
	}
	selector, ok := selectors[0].(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("workload: DeviceClass %q has malformed selector", class.GetName())
	}
	cel, ok := selector["cel"].(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("workload: DeviceClass %q has unsupported selector", class.GetName())
	}
	expression, _ := cel["expression"].(string)
	expression = strings.TrimSpace(expression)
	const prefix = "device.driver == "
	if !strings.HasPrefix(expression, prefix) {
		return "", fmt.Errorf("workload: DeviceClass %q selector must be exactly device.driver == '<driver>'", class.GetName())
	}
	quoted := strings.TrimSpace(strings.TrimPrefix(expression, prefix))
	if len(quoted) < 3 || (quoted[0] != '\'' && quoted[0] != '"') || quoted[len(quoted)-1] != quoted[0] || strings.ContainsRune(quoted[1:len(quoted)-1], rune(quoted[0])) {
		return "", fmt.Errorf("workload: DeviceClass %q selector must be exactly device.driver == '<driver>'", class.GetName())
	}
	return quoted[1 : len(quoted)-1], nil
}

func draCapacity(driver string, slices, claims []unstructured.Unstructured) (available, total int64, err error) {
	available, total, _, err = draCapacitySnapshot(driver, slices, claims)
	return available, total, err
}

func draCapacitySnapshot(driver string, slices, claims []unstructured.Unstructured) (available, total int64, freeByNode map[string]int64, err error) {
	return draCapacitySnapshotForType(driver, "", slices, claims)
}

func draCapacitySnapshotForType(driver, acceleratorType string, slices, claims []unstructured.Unstructured) (available, total int64, freeByNode map[string]int64, err error) {
	devices := make(map[string]string)
	for _, slice := range slices {
		sliceDriver, found, err := unstructured.NestedString(slice.Object, "spec", "driver")
		if err != nil || !found {
			return 0, 0, nil, fmt.Errorf("workload: ResourceSlice %q has no valid spec.driver", slice.GetName())
		}
		if sliceDriver != driver {
			continue
		}
		node, _, err := unstructured.NestedString(slice.Object, "spec", "nodeName")
		if err != nil {
			return 0, 0, nil, fmt.Errorf("workload: ResourceSlice %q has invalid spec.nodeName", slice.GetName())
		}
		pool, found, err := unstructured.NestedString(slice.Object, "spec", "pool", "name")
		if err != nil || !found || pool == "" {
			return 0, 0, nil, fmt.Errorf("workload: ResourceSlice %q has no valid spec.pool.name", slice.GetName())
		}
		items, found, err := unstructured.NestedSlice(slice.Object, "spec", "devices")
		if err != nil || !found {
			return 0, 0, nil, fmt.Errorf("workload: ResourceSlice %q has no device inventory", slice.GetName())
		}
		for _, item := range items {
			device, ok := item.(map[string]interface{})
			if !ok {
				return 0, 0, nil, fmt.Errorf("workload: ResourceSlice %q contains malformed device", slice.GetName())
			}
			name, ok := device["name"].(string)
			if !ok || name == "" {
				return 0, 0, nil, fmt.Errorf("workload: ResourceSlice %q contains device without name", slice.GetName())
			}
			key := pool + "/" + name
			if _, duplicate := devices[key]; duplicate {
				return 0, 0, nil, fmt.Errorf("workload: DRA driver %q publishes duplicate device %q", driver, key)
			}
			devices[key] = node
		}
	}
	allocated := make(map[string]struct{})
	for _, claim := range claims {
		results, found, err := unstructured.NestedSlice(claim.Object, "status", "allocation", "devices", "results")
		if err != nil {
			return 0, 0, nil, fmt.Errorf("workload: ResourceClaim %s/%s has malformed allocation: %w", claim.GetNamespace(), claim.GetName(), err)
		}
		if !found {
			continue
		}
		for _, item := range results {
			result, ok := item.(map[string]interface{})
			if !ok {
				return 0, 0, nil, fmt.Errorf("workload: ResourceClaim %s/%s has malformed allocation result", claim.GetNamespace(), claim.GetName())
			}
			resultDriver, _ := result["driver"].(string)
			if resultDriver != driver {
				continue
			}
			pool, poolOK := result["pool"].(string)
			device, deviceOK := result["device"].(string)
			if !poolOK || !deviceOK || pool == "" || device == "" {
				return 0, 0, nil, fmt.Errorf("workload: ResourceClaim %s/%s has incomplete allocation identity", claim.GetNamespace(), claim.GetName())
			}
			key := pool + "/" + device
			if _, exists := devices[key]; !exists {
				if acceleratorType != "" {
					continue
				}
				return 0, 0, nil, fmt.Errorf("workload: allocated DRA device %q is absent from current ResourceSlices", key)
			}
			allocated[key] = struct{}{}
		}
	}
	total = int64(len(devices))
	freeByNode = make(map[string]int64)
	for key, node := range devices {
		if node != "" {
			if _, inUse := allocated[key]; !inUse {
				freeByNode[node]++
			}
		}
	}
	return total - int64(len(allocated)), total, freeByNode, nil
}

// ensureResourceClaimTemplate creates the companion resource a DRA-backed Pod references. A
// retained resource from a different desired specification is removed; the next stateless pass
// recreates it before creating the Job.
func (c *JobWorkloadClient) ensureResourceClaimTemplate(ctx context.Context, job *batchv1.Job, exp *domain.Experiment) error {
	placement, err := c.ResolveAcceleratorPlacement(ctx, exp.AcceleratorType)
	if err != nil {
		return err
	}
	deviceClassName := placement.DeviceClassName
	if deviceClassName == "" {
		return fmt.Errorf("accelerator type %q is not DRA-backed on this cluster", exp.AcceleratorType)
	}
	// The count one POD claims, read off the pod template this Job was compiled with — the group's
	// per-replica count for a grouped job, the per-node count otherwise. Never exp.AcceleratorCount,
	// which is the whole job's total across every node: claiming that per pod asks each node for
	// the entire job's hardware.
	perPod, convErr := strconv.Atoi(job.Spec.Template.Annotations[AcceleratorCountAnnotation])
	if convErr != nil || perPod < 1 {
		return fmt.Errorf("accelerator type %q has invalid per-pod count %q", exp.AcceleratorType, job.Spec.Template.Annotations[AcceleratorCountAnnotation])
	}

	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": resourceClaimTemplateGVR.GroupVersion().String(),
		"kind":       "ResourceClaimTemplate",
		"metadata": map[string]interface{}{
			"name": resourceClaimTemplateName(job.Name), "namespace": HypothesisLoopNamespace,
			"labels":      job.Labels,
			"annotations": map[string]interface{}{DesiredSpecHashAnnotation: job.Annotations[DesiredSpecHashAnnotation]},
		},
		"spec": map[string]interface{}{
			"metadata": map[string]interface{}{"labels": job.Labels},
			"spec": map[string]interface{}{"devices": map[string]interface{}{"requests": []interface{}{
				map[string]interface{}{"name": draClaimName, "exactly": map[string]interface{}{
					"deviceClassName": deviceClassName, "allocationMode": "ExactCount", "count": int64(perPod),
				}},
			}}},
		},
	}}

	_, err = c.dyn.Resource(resourceClaimTemplateGVR).Namespace(HypothesisLoopNamespace).Create(ctx, obj, metav1.CreateOptions{})
	if !errors.IsAlreadyExists(err) {
		return err
	}
	existing, err := c.dyn.Resource(resourceClaimTemplateGVR).Namespace(HypothesisLoopNamespace).Get(ctx, obj.GetName(), metav1.GetOptions{})
	if err != nil {
		return err
	}
	if existing.GetDeletionTimestamp() == nil && existing.GetAnnotations()[DesiredSpecHashAnnotation] == job.Annotations[DesiredSpecHashAnnotation] &&
		existing.GetLabels()[workloadkeys.ManagedBy] == workloadkeys.ManagedByValue && existing.GetLabels()[workloadkeys.ExperimentID] == exp.ID {
		return nil
	}
	if err := c.dyn.Resource(resourceClaimTemplateGVR).Namespace(HypothesisLoopNamespace).Delete(ctx, obj.GetName(), metav1.DeleteOptions{}); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete drifted ResourceClaimTemplate: %w", err)
	}
	return fmt.Errorf("drifted ResourceClaimTemplate %s removed; retry creation", obj.GetName())
}

func (c *JobWorkloadClient) draTemplateMatches(ctx context.Context, jobName, experimentID string, wanted bool, desiredHash string) (bool, error) {
	template, err := c.dyn.Resource(resourceClaimTemplateGVR).Namespace(HypothesisLoopNamespace).Get(ctx, resourceClaimTemplateName(jobName), metav1.GetOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return false, err
	}
	return wanted == (err == nil) && (err != nil || (template.GetAnnotations()[DesiredSpecHashAnnotation] == desiredHash &&
		template.GetLabels()[workloadkeys.ManagedBy] == workloadkeys.ManagedByValue && template.GetLabels()[workloadkeys.ExperimentID] == experimentID)), nil
}

func (c *JobWorkloadClient) addManagedDRAIDs(ctx context.Context, ids map[string]struct{}) error {
	templates, err := c.dyn.Resource(resourceClaimTemplateGVR).Namespace(HypothesisLoopNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", workloadkeys.ManagedBy, workloadkeys.ManagedByValue),
	})
	if err != nil {
		return err
	}
	for _, template := range templates.Items {
		id := template.GetLabels()[workloadkeys.ExperimentID]
		if id == "" {
			log.Printf("workload: skipping managed ResourceClaimTemplate %q with no experiment identity", template.GetName())
			continue
		}
		ids[id] = struct{}{}
	}
	return nil
}

// deleteDRAResource removes every ResourceClaimTemplate belonging to experimentID. Listed by
// label rather than deleted by a computed name because a grouped experiment has one template per
// accelerator-holding group, and a name derived from the experiment id alone would only ever
// reach one of them.
func (c *JobWorkloadClient) deleteDRAResource(ctx context.Context, experimentID string) error {
	templates, err := c.dyn.Resource(resourceClaimTemplateGVR).Namespace(HypothesisLoopNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s,%s=%s", workloadkeys.ManagedBy, workloadkeys.ManagedByValue, workloadkeys.ExperimentID, experimentID),
	})
	if err != nil {
		return err
	}
	var errs []error
	for _, template := range templates.Items {
		delErr := c.dyn.Resource(resourceClaimTemplateGVR).Namespace(HypothesisLoopNamespace).
			Delete(ctx, template.GetName(), metav1.DeleteOptions{})
		if delErr != nil && !errors.IsNotFound(delErr) {
			// One template failing must not strand the others (important.md #19).
			errs = append(errs, delErr)
		}
	}
	return stderrors.Join(errs...)
}
