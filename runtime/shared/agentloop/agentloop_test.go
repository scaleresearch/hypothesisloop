package agentloop

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
)

func TestOrphanAuxiliaryIDsOnlyDeletesResourcesWithNoDesiredOrActualJob(t *testing.T) {
	desired := map[string]*clusterapi.DesiredJob{"desired": {ID: "desired"}}
	actual := map[string]bool{"actual": true}
	got := orphanAuxiliaryIDs(desired, actual, []string{"desired", "actual", "orphan"})
	if !reflect.DeepEqual(got, []string{"orphan"}) {
		t.Fatalf("orphanAuxiliaryIDs = %v, want [orphan]", got)
	}
}

// stalledExecutor never answers a read until its context ends.
type stalledExecutor struct{ *captureExecutor }

func (stalledExecutor) GetLiveCPUCapacity(ctx context.Context) (float64, float64, error) {
	<-ctx.Done()
	return 0, 0, ctx.Err()
}

func (stalledExecutor) ListManagedJobsForStatus(ctx context.Context) ([]string, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// One stalled call costs its pass and nothing more: the pass's deadline cancels it.
func TestAStalledCallEndsWithItsPass(t *testing.T) {
	server := newUndesiredStateServer(t)
	defer server.Close()
	exec := stalledExecutor{&captureExecutor{}}
	agent := newTestAgent(server, exec.captureExecutor)
	agent.Executor = exec
	agent.ReconcileInterval, agent.StatusInterval = 10*time.Millisecond, 10*time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := agent.reconcileOnce(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("reconcileOnce = %v, want the pass's deadline", err)
	}
	agent.reportChangedStatuses(ctx)
	if ctx.Err() != nil {
		t.Fatal("a stalled call outlived its pass")
	}
}
