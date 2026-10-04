package k8sexec

import (
	"context"
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
)

// nodePodCache holds every node and every pod of the cluster, listed once and kept current by a
// watch. The capacity the agent reports on every reconcile pass is counted from them several times
// over, and listing them from the API server each time moved the whole cluster's nodes and pods
// across the wire many times a second for as long as the agent ran.
type nodePodCache struct {
	start  sync.Once
	nodes  corelisters.NodeLister
	pods   corelisters.PodLister
	synced []cache.InformerSynced
}

// cachedNodesAndPods starts the cache on first use and waits, bounded by ctx, for it to hold the
// cluster's state. The informers live as long as the process.
func (c *JobWorkloadClient) cachedNodesAndPods(ctx context.Context) ([]*corev1.Node, []*corev1.Pod, error) {
	nc := &c.nodePods
	nc.start.Do(func() {
		factory := informers.NewSharedInformerFactoryWithOptions(c.kube, 0, informers.WithTransform(dropManagedFields))
		nodes, pods := factory.Core().V1().Nodes(), factory.Core().V1().Pods()
		nc.nodes, nc.pods = nodes.Lister(), pods.Lister()
		nc.synced = []cache.InformerSynced{nodes.Informer().HasSynced, pods.Informer().HasSynced}
		factory.Start(make(chan struct{}))
	})
	if !cache.WaitForCacheSync(ctx.Done(), nc.synced...) {
		return nil, nil, fmt.Errorf("workload: nodes and pods not yet read from the API server: %w", context.Cause(ctx))
	}
	nodes, err := nc.nodes.List(labels.Everything())
	if err != nil {
		return nil, nil, fmt.Errorf("workload: list nodes: %w", err)
	}
	pods, err := nc.pods.List(labels.Everything())
	if err != nil {
		return nil, nil, fmt.Errorf("workload: list pods: %w", err)
	}
	return nodes, pods, nil
}

func dropManagedFields(obj any) (any, error) {
	if m, err := meta.Accessor(obj); err == nil {
		m.SetManagedFields(nil)
	}
	return obj, nil
}
