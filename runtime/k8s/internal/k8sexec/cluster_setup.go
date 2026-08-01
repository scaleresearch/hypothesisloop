package k8sexec

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SetupCluster ensures the namespace and the two native PriorityClasses exist. There is no
// operator to install and no queue/cohort/flavor topology to reconcile — capacity accounting
// happens in Go (services/scheduler), not as Kubernetes objects.
func (c *JobWorkloadClient) SetupCluster(ctx context.Context) error {
	if err := c.ensureNamespace(ctx, HypothesisLoopNamespace); err != nil {
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
			Description:   "HypothesisLoop capacity tier priority class (managed in-code, not by an operator).",
		}
		_, err := c.kube.SchedulingV1().PriorityClasses().Create(ctx, obj, metav1.CreateOptions{})
		if err != nil && !errors.IsAlreadyExists(err) {
			return fmt.Errorf("workload: create PriorityClass %s: %w", pc.name, err)
		}
	}
	return nil
}
