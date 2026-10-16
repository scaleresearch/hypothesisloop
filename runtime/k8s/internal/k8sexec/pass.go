package k8sexec

import (
	"context"
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/scaleresearch/hypothesisloop/runtime/shared/agentexec"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/workloadkeys"
)

// clusterRead is one look at every node and every pod of the cluster.
type clusterRead struct {
	nodes []corev1.Node
	pods  []corev1.Pod
}

// ForPass returns a client that answers every node and pod read of one pass from a single list
// of each, made when the pass first asks.
func (c *JobWorkloadClient) ForPass(ctx context.Context) (agentexec.Executor, error) {
	pass := *c
	pass.read = sync.OnceValues(func() (clusterRead, error) { return c.readCluster(ctx) })
	return &pass, nil
}

// cluster is the pass's read, or a fresh one outside a pass.
func (c *JobWorkloadClient) cluster(ctx context.Context) (clusterRead, error) {
	if c.read == nil {
		return c.readCluster(ctx)
	}
	return c.read()
}

func (c *JobWorkloadClient) readCluster(ctx context.Context) (clusterRead, error) {
	nodes, err := c.kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return clusterRead{}, fmt.Errorf("workload: list nodes: %w", err)
	}
	pods, err := c.kube.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return clusterRead{}, fmt.Errorf("workload: list pods: %w", err)
	}
	return clusterRead{nodes: nodes.Items, pods: pods.Items}, nil
}

// experimentPods is every pod of the experiment in the jobs namespace, of every attempt.
func (c *JobWorkloadClient) experimentPods(ctx context.Context, experimentID string) ([]corev1.Pod, error) {
	read, err := c.cluster(ctx)
	if err != nil {
		return nil, err
	}
	var pods []corev1.Pod
	for _, p := range read.pods {
		if p.Namespace == HypothesisLoopNamespace && p.Labels[workloadkeys.ExperimentID] == experimentID {
			pods = append(pods, p)
		}
	}
	return pods, nil
}
