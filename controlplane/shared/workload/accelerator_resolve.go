package workload

import (
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// An accelerator type is a fact its driver publishes (see domain.AcceleratorType) — it says what
// the hardware IS, not how Kubernetes is asked for it. This file is the single place that turns
// the former into the latter, and it does so by reading the cluster's live inventory rather than
// from any configured table. That is what keeps one name valid everywhere: nothing restates the
// hardware's identity, so nothing can restate it wrongly.
//
// Two mechanisms exist, and which one applies is discovered, not declared:
//
//   - DRA. The type's key domain matches a DeviceClass's driver (tenstorrent.com), and the
//     type's attribute appears on that driver's devices. The job gets a ResourceClaimTemplate.
//   - Device plugin. The type's key is a node label (nvidia.com/gpu.product), and nodes carrying
//     it advertise an extended resource in the same domain. The job gets a resource request plus
//     node affinity on that exact label.

// AcceleratorPlacement is how one accelerator type is actually requested on this cluster.
// Exactly one of DeviceClassName (DRA) or ResourceName (device plugin) is set.
type AcceleratorPlacement struct {
	// DeviceClassName is the DRA DeviceClass to claim from, when this type is DRA-backed.
	DeviceClassName string
	// ResourceName is the extended resource to request (e.g. "nvidia.com/gpu"), when this type
	// is device-plugin-backed.
	ResourceName string
	// NodeLabel is the label key/value that pins placement to nodes carrying this exact
	// hardware. Set only for the device-plugin path: an extended resource request alone gets
	// *an* accelerator node, never *this model*, because every model in a vendor's domain
	// advertises the same resource name. Empty for DRA, where the claim already selects the
	// device.
	NodeLabelKey   string
	NodeLabelValue string
}

// UsesDRA reports which of the two request mechanisms this placement uses.
func (p AcceleratorPlacement) UsesDRA() bool { return p.DeviceClassName != "" }

// ResolveAcceleratorPlacement finds how to request acceleratorType on this cluster, by matching
// it against live DRA device attributes and node labels.
//
// Fails loudly rather than guessing. A type that resolves to nothing means no hardware here
// publishes that fact — the job would otherwise queue forever with no error, which is exactly
// the failure this returns instead. An ambiguous resolution (a vendor domain advertising more
// than one extended resource on the matching nodes) also errors: silently picking one would
// make billing and placement disagree about what ran.
func (c *JobWorkloadClient) ResolveAcceleratorPlacement(ctx context.Context, acceleratorType domain.AcceleratorType) (AcceleratorPlacement, error) {
	if err := acceleratorType.Validate(); err != nil {
		return AcceleratorPlacement{}, fmt.Errorf("workload: %w", err)
	}

	if placement, found, err := c.resolveDRAPlacement(ctx, acceleratorType); err != nil || found {
		return placement, err
	}
	placement, found, err := c.resolveDevicePluginPlacement(ctx, acceleratorType)
	if err != nil {
		return AcceleratorPlacement{}, err
	}
	if !found {
		return AcceleratorPlacement{}, fmt.Errorf(
			"workload: no hardware on this cluster publishes %q — no DRA device attribute and no node label matches it",
			string(acceleratorType))
	}
	return placement, nil
}

// resolveDRAPlacement looks for a DeviceClass whose driver is the type's vendor domain and whose
// devices actually publish the type's attribute.
func (c *JobWorkloadClient) resolveDRAPlacement(ctx context.Context, acceleratorType domain.AcceleratorType) (AcceleratorPlacement, bool, error) {
	classes, err := c.dyn.Resource(deviceClassGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return AcceleratorPlacement{}, false, fmt.Errorf("workload: list DRA DeviceClasses: %w", err)
	}
	if len(classes.Items) == 0 {
		return AcceleratorPlacement{}, false, nil
	}
	slices, err := c.dyn.Resource(resourceSliceGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return AcceleratorPlacement{}, false, fmt.Errorf("workload: list DRA ResourceSlices: %w", err)
	}

	for _, class := range classes.Items {
		driver, err := deviceClassDriver(class)
		if err != nil {
			return AcceleratorPlacement{}, false, err
		}
		if driver != acceleratorType.Domain() {
			continue
		}
		for _, slice := range slices.Items {
			sliceDriver, _, _ := unstructured.NestedString(slice.Object, "spec", "driver")
			if sliceDriver != driver {
				continue
			}
			devices, _, _ := unstructured.NestedSlice(slice.Object, "spec", "devices")
			for _, item := range devices {
				device, ok := item.(map[string]interface{})
				if !ok {
					continue
				}
				if acceleratorType.MatchesAttributes(driver, deviceAttributeStrings(device)) {
					return AcceleratorPlacement{DeviceClassName: class.GetName()}, true, nil
				}
			}
		}
	}
	return AcceleratorPlacement{}, false, nil
}

// resolveDevicePluginPlacement looks for nodes carrying the type's label, then for the extended
// resource those nodes advertise in the same vendor domain.
func (c *JobWorkloadClient) resolveDevicePluginPlacement(ctx context.Context, acceleratorType domain.AcceleratorType) (AcceleratorPlacement, bool, error) {
	nodes, _, err := c.listSchedulableNodesAndPods(ctx)
	if err != nil {
		return AcceleratorPlacement{}, false, err
	}
	resources := map[string]bool{}
	matched := false
	for _, node := range nodes {
		if !acceleratorType.MatchesLabels(node.Labels) {
			continue
		}
		matched = true
		for name := range node.Status.Allocatable {
			if resourceDomain(string(name)) == acceleratorType.Domain() {
				resources[string(name)] = true
			}
		}
	}
	if !matched {
		return AcceleratorPlacement{}, false, nil
	}
	if len(resources) == 0 {
		return AcceleratorPlacement{}, false, fmt.Errorf(
			"workload: nodes publish %q but advertise no %s extended resource to request it with",
			string(acceleratorType), acceleratorType.Domain())
	}
	if len(resources) > 1 {
		names := make([]string, 0, len(resources))
		for name := range resources {
			names = append(names, name)
		}
		sort.Strings(names)
		return AcceleratorPlacement{}, false, fmt.Errorf(
			"workload: %q is ambiguous — its nodes advertise several %s extended resources (%v); cannot tell which one backs it",
			string(acceleratorType), acceleratorType.Domain(), names)
	}
	var resourceName string
	for name := range resources {
		resourceName = name
	}
	return AcceleratorPlacement{
		ResourceName:   resourceName,
		NodeLabelKey:   acceleratorType.Key(),
		NodeLabelValue: acceleratorType.Value(),
	}, true, nil
}

// deviceAttributeStrings flattens a ResourceSlice device's attributes to their string values.
// Non-string attributes (ints, bools, versions) are rendered so they can still be matched — a
// driver publishing boardType as an int is still publishing a fact about the hardware.
func deviceAttributeStrings(device map[string]interface{}) map[string]string {
	raw, _, _ := unstructured.NestedMap(device, "attributes")
	out := make(map[string]string, len(raw))
	for name, value := range raw {
		entry, ok := value.(map[string]interface{})
		if !ok {
			continue
		}
		for _, v := range entry {
			out[name] = fmt.Sprintf("%v", v)
			break
		}
	}
	return out
}

// resourceDomain returns the vendor domain of an extended resource name
// ("nvidia.com" for "nvidia.com/gpu"), or "" for a core resource like cpu/memory.
func resourceDomain(name string) string {
	for i := 0; i < len(name); i++ {
		if name[i] == '/' {
			return name[:i]
		}
	}
	return ""
}

// acceleratorNodeAffinity pins a device-plugin-backed job to nodes publishing the exact label
// its accelerator type names. Nil for DRA (the claim selects the device) or when no label
// applies.
func acceleratorNodeAffinity(placement AcceleratorPlacement) *corev1.NodeAffinity {
	if placement.NodeLabelKey == "" {
		return nil
	}
	return &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchExpressions: []corev1.NodeSelectorRequirement{{
					Key:      placement.NodeLabelKey,
					Operator: corev1.NodeSelectorOpIn,
					Values:   []string{placement.NodeLabelValue},
				}},
			}},
		},
	}
}

// resolvePlacementFor resolves exp's accelerator placement, or the zero placement for a job that
// requests no accelerator.
func (c *JobWorkloadClient) resolvePlacementFor(ctx context.Context, exp *domain.Experiment) (AcceleratorPlacement, error) {
	if exp.Job.AcceleratorCount <= 0 {
		return AcceleratorPlacement{}, nil
	}
	return c.ResolveAcceleratorPlacement(ctx, exp.AcceleratorType)
}
