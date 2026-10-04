package k8sexec

import (
	"context"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// Every reconcile pass reads capacity several times. The nodes and pods behind it are listed once
// for the life of the agent and kept current by a watch.
func TestCapacityReadsListNodesAndPodsOnceAndFollowTheWatch(t *testing.T) {
	kube := fake.NewClientset(totalCapacityTestNode("node-a", 8, 32), requestingPod("cni-agent", "node-a", false, 500))
	podsWatched, watchedOnce := make(chan struct{}), sync.Once{}
	kube.PrependWatchReactor("pods", func(action k8stesting.Action) (bool, watch.Interface, error) {
		w, err := kube.Tracker().Watch(action.GetResource(), action.GetNamespace())
		watchedOnce.Do(func() { close(podsWatched) })
		return true, w, err
	})
	c := &JobWorkloadClient{kube: kube}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	freeCPU := func() int64 {
		t.Helper()
		byNode, err := c.GetLiveNodeResourceCapacity(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return byNode["node-a"][domain.NodeResourceCPUMillicores]
	}
	for range 10 {
		if got := freeCPU(); got != 7500 {
			t.Fatalf("free cpu = %dm, want 7500m", got)
		}
		if _, _, err := c.GetLiveCPUCapacity(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := c.GetNodeTotalCapacity(ctx); err != nil {
			t.Fatal(err)
		}
	}
	lists := map[string]int{}
	for _, action := range kube.Actions() {
		if action.GetVerb() == "list" {
			lists[action.GetResource().Resource]++
		}
	}
	if lists["nodes"] != 1 || lists["pods"] != 1 {
		t.Fatalf("lists after 30 capacity reads = %v, want nodes and pods listed once each", lists)
	}

	<-podsWatched
	if _, err := kube.CoreV1().Pods("default").Create(ctx, requestingPod("job", "node-a", true, 2000), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	for freeCPU() != 5500 {
		if ctx.Err() != nil {
			t.Fatalf("free cpu = %dm after a pod requesting 2000m started, want 5500m", freeCPU())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
