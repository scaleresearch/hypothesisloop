package workload

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var defaultFlavorOrder = []string{"flavor-t4", "flavor-l40", "flavor-a100", "flavor-h100", "flavor-h200"}

var defaultNameByFlavor = map[string]string{
	"flavor-t4": "T4", "flavor-l40": "L40", "flavor-a100": "A100",
	"flavor-h100": "H100", "flavor-h200": "H200",
}

var defaultGPUsByFlavor = map[string]int{
	"flavor-t4": 64, "flavor-l40": 16, "flavor-a100": 8, "flavor-h100": 4, "flavor-h200": 2,
}

func (c *JobWorkloadClient) flavorOrder() []string {
	if c.pcfg != nil {
		return c.pcfg.FlavorOrder
	}
	return defaultFlavorOrder
}

func (c *JobWorkloadClient) nameByFlavor() map[string]string {
	if c.pcfg != nil {
		return c.pcfg.NameByFlavor
	}
	return defaultNameByFlavor
}

func (c *JobWorkloadClient) gpusByFlavor() map[string]int {
	if c.pcfg != nil {
		return c.pcfg.GPUsByFlavor
	}
	return defaultGPUsByFlavor
}

// gpuNominalCapacity returns the nominal GPU slot count per flavor from config (or the
// PoC defaults). This is the only resource dimension the scheduler admits/preempts on —
// CPU, memory, and storage are not modeled.
func (c *JobWorkloadClient) gpuNominalCapacity() map[string]int64 {
	gpus := c.gpusByFlavor()
	out := make(map[string]int64, len(gpus))
	for flavor, count := range gpus {
		out[flavor] = int64(count)
	}
	return out
}

// SetupCluster ensures the namespace and the two native PriorityClasses exist. There is no
// operator to install and no queue/cohort/flavor topology to reconcile — capacity accounting
// happens in Go (services/scheduler), not as Kubernetes objects.
func (c *JobWorkloadClient) SetupCluster(ctx context.Context) error {
	if err := c.ensureNamespace(ctx, OpenResearchNamespace); err != nil {
		return err
	}
	return c.ensurePriorityClasses(ctx)
}

func (c *JobWorkloadClient) ensureNamespace(ctx context.Context, name string) error {
	_, err := c.kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}, metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("workload: create namespace %s: %w", name, err)
	}
	return nil
}

// ensurePriorityClasses creates the two native scheduling.k8s.io/v1 PriorityClass objects
// used to order/preempt pods at the k8s scheduler level. These are cluster-scoped and
// idempotent to (re-)create.
func (c *JobWorkloadClient) ensurePriorityClasses(ctx context.Context) error {
	classes := []struct {
		name  string
		value int32
	}{
		{PriorityClassGuaranteed, priorityValueGuaranteed},
		{PriorityClassBurst, priorityValueBurst},
	}
	for _, pc := range classes {
		obj := &schedulingv1.PriorityClass{
			ObjectMeta:    metav1.ObjectMeta{Name: pc.name},
			Value:         pc.value,
			GlobalDefault: false,
			Description:   "OpenResearch capacity tier priority class (managed in-code, not by an operator).",
		}
		_, err := c.kube.SchedulingV1().PriorityClasses().Create(ctx, obj, metav1.CreateOptions{})
		if err != nil && !errors.IsAlreadyExists(err) {
			return fmt.Errorf("workload: create PriorityClass %s: %w", pc.name, err)
		}
	}
	return nil
}
