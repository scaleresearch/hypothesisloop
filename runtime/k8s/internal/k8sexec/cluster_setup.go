package k8sexec

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SetupCluster checks that what the chart installs for jobs is there: the namespace they run in
// and the two PriorityClasses their pods name. The agent creates none of it, so it holds no
// right to create a namespace or a PriorityClass, and a cluster without them is said so at start
// instead of on the first job.
func (c *JobWorkloadClient) SetupCluster(ctx context.Context) error {
	if _, err := c.kube.CoreV1().Namespaces().Get(ctx, HypothesisLoopNamespace, metav1.GetOptions{}); err != nil {
		return fmt.Errorf("workload: namespace %s, which the runtime chart installs: %w", HypothesisLoopNamespace, err)
	}
	for _, name := range []string{PriorityClassGuaranteed, PriorityClassBurst} {
		if _, err := c.kube.SchedulingV1().PriorityClasses().Get(ctx, name, metav1.GetOptions{}); err != nil {
			return fmt.Errorf("workload: PriorityClass %s, which the runtime chart installs: %w", name, err)
		}
	}
	return nil
}
