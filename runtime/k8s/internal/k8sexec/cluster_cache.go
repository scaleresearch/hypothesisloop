package k8sexec

import (
	"context"
	"fmt"
	"sync"
	"time"

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

	nodesSynced, podsSynced cache.InformerSynced
}

// startCache starts the informers once. They live as long as the process.
func (c *JobWorkloadClient) startCache() *nodePodCache {
	nc := &c.nodePods
	nc.start.Do(func() {
		factory := informers.NewSharedInformerFactoryWithOptions(c.kube, 0, informers.WithTransform(dropManagedFields))
		nodes, pods := factory.Core().V1().Nodes(), factory.Core().V1().Pods()
		nc.nodes, nc.pods = nodes.Lister(), pods.Lister()
		nc.nodesSynced, nc.podsSynced = nodes.Informer().HasSynced, pods.Informer().HasSynced
		nc.synced = []cache.InformerSynced{nc.nodesSynced, nc.podsSynced}
		factory.Start(make(chan struct{}))
	})
	return nc
}

// SyncCache reads every node and then every pod of the cluster into the cache before the agent
// first reports, saying each step as it starts and ends, and fails naming the step that did not
// end within timeout. Listing every pod of a large cluster, or through a slow link to its API
// server, takes minutes, which would otherwise pass in silence before the first contact.
func (c *JobWorkloadClient) SyncCache(ctx context.Context, timeout time.Duration, log func(format string, args ...any)) error {
	nc := c.startCache()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	started := time.Now()
	for _, step := range []struct {
		what   string
		synced cache.InformerSynced
		count  func() (int, error)
	}{
		{"nodes", nc.nodesSynced, func() (int, error) { n, err := nc.nodes.List(labels.Everything()); return len(n), err }},
		{"pods", nc.podsSynced, func() (int, error) { p, err := nc.pods.List(labels.Everything()); return len(p), err }},
	} {
		log("syncing %s from the API server", step.what)
		if !cache.WaitForCacheSync(ctx.Done(), step.synced) {
			return fmt.Errorf("the %s of this cluster were not read from its API server within %s: the API server is slow or unreachable from this pod, or the agent may not list %s (its ClusterRole): %w",
				step.what, timeout, step.what, context.Cause(ctx))
		}
		n, err := step.count()
		if err != nil {
			return fmt.Errorf("workload: list %s: %w", step.what, err)
		}
		log("synced %d %s in %s", n, step.what, time.Since(started).Round(time.Millisecond))
	}
	return nil
}

// cachedNodesAndPods starts the cache on first use and waits, bounded by ctx, for it to hold the
// cluster's state.
func (c *JobWorkloadClient) cachedNodesAndPods(ctx context.Context) ([]*corev1.Node, []*corev1.Pod, error) {
	nc := c.startCache()
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
