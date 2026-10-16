package k8sexec

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
)

// The agent says each step of reading the cluster before its first report, so minutes spent
// listing the pods of a large cluster are not minutes of silence.
func TestTheCacheSyncSaysEachStep(t *testing.T) {
	kube := chartKube(t, totalCapacityTestNode("node-a", 8, 32), requestingPod("cni-agent", "node-a", false, 500))
	c := &JobWorkloadClient{kube: kube}
	var mu sync.Mutex
	var logged []string
	log := func(format string, args ...any) {
		mu.Lock()
		logged = append(logged, fmt.Sprintf(format, args...))
		mu.Unlock()
	}
	if err := c.SyncCache(context.Background(), 10*time.Second, log); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"syncing nodes from the API server", "synced 1 nodes in", "syncing pods from the API server", "synced 1 pods in"}
	if len(logged) != len(want) {
		t.Fatalf("logged %q, want %q", logged, want)
	}
	for i := range want {
		if !strings.HasPrefix(logged[i], want[i]) {
			t.Errorf("line %d = %q, want it to start %q", i, logged[i], want[i])
		}
	}
}

// A read that never ends is bounded, and the error names what was not read.
func TestACacheSyncThatDoesNotEndFailsNamingTheStep(t *testing.T) {
	kube := chartKube(t, totalCapacityTestNode("node-a", 8, 32))
	kube.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("the API server is far away")
	})
	c := &JobWorkloadClient{kube: kube}
	err := c.SyncCache(context.Background(), 300*time.Millisecond, func(string, ...any) {})
	if err == nil || !strings.Contains(err.Error(), "the pods of this cluster were not read") || !strings.Contains(err.Error(), "300ms") {
		t.Fatalf("SyncCache = %v, want it bounded and naming the pods", err)
	}
}
