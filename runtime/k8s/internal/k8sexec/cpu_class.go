package k8sexec

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/scaleresearch/hypothesisloop/runtime/shared/workloadkeys"
)

const (
	// CPUClassLabel names a node's CPU class. With CPUClassResource it makes the class a type
	// like any device-plugin accelerator: capacity is reported as "cpu.hypothesisloop.dev/class=
	// <name>", and a job on it requests the resource on nodes carrying the label.
	CPUClassLabel = workloadkeys.CPUClassLabel
	// CPUClassResource is one core of the node's CPU class.
	CPUClassResource corev1.ResourceName = "cpu.hypothesisloop.dev/core"
)

func validCPUClass(name string) error {
	if problems := validation.IsValidLabelValue(name); len(problems) > 0 {
		return fmt.Errorf("workload: CPU class %q is no node label value: %s", name, strings.Join(problems, "; "))
	}
	return nil
}

// PublishCPUClass makes every schedulable node offer its cores as units of the configured CPU
// class: one unit per whole allocatable core, so a job holding k units holds k cores and k parts
// of the node's memory and storage. It writes a node only when what the node says differs, and
// is called again from time to time because nodes join and their allocatable changes. With no
// class configured it does nothing.
func (c *JobWorkloadClient) PublishCPUClass(ctx context.Context) error {
	if c.cpuClass == "" {
		return nil
	}
	nodes, err := c.kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("workload: publish CPU class: list nodes: %w", err)
	}
	for _, node := range nodes.Items {
		if !schedulableNode(node) {
			continue
		}
		cores := resource.NewQuantity(node.Status.Allocatable.Cpu().MilliValue()/1000, resource.DecimalSI)
		capacity, allocatable := node.Status.Capacity[CPUClassResource], node.Status.Allocatable[CPUClassResource]
		if capacity.Cmp(*cores) != 0 || allocatable.Cmp(*cores) != 0 {
			units := map[string]string{string(CPUClassResource): cores.String()}
			patch, err := json.Marshal(map[string]any{"status": map[string]any{"capacity": units, "allocatable": units}})
			if err != nil {
				return err
			}
			if _, err := c.kube.CoreV1().Nodes().Patch(ctx, node.Name, types.MergePatchType, patch, metav1.PatchOptions{}, "status"); err != nil {
				return fmt.Errorf("workload: publish CPU class: %s units on node %s: %w", cores, node.Name, err)
			}
		}
		if node.Labels[CPUClassLabel] != c.cpuClass {
			patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"labels": map[string]string{CPUClassLabel: c.cpuClass}}})
			if err != nil {
				return err
			}
			if _, err := c.kube.CoreV1().Nodes().Patch(ctx, node.Name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
				return fmt.Errorf("workload: publish CPU class: label node %s: %w", node.Name, err)
			}
		}
	}
	return nil
}
