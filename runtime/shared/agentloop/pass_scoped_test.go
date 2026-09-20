package agentloop

import (
	"context"
	"errors"
	"testing"

	"github.com/scaleresearch/hypothesisloop/runtime/shared/agentexec"
)

// scopedExecutor hands out its captureExecutor once per pass, and is unusable in any other way:
// the embedded Executor is nil, so a call the loop makes on the agent's own executor panics.
type scopedExecutor struct {
	agentexec.Executor
	inner  *captureExecutor
	passes int
	err    error
}

func (e *scopedExecutor) ForPass(context.Context) (agentexec.Executor, error) {
	e.passes++
	return e.inner, e.err
}

// A reconcile pass, the last status push it makes before a delete included, and a status push of
// the status loop each ask a pass-scoped executor for one executor and use nothing else.
func TestAPassScopedExecutorIsAskedOncePerPass(t *testing.T) {
	server := newUndesiredStateServer(t)
	defer server.Close()
	scoped := &scopedExecutor{inner: &captureExecutor{managed: []string{"exp-gone"}}}
	agent := newTestAgent(server, nil)
	agent.Executor = scoped

	if err := agent.reconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if scoped.passes != 1 || len(scoped.inner.deleted) != 1 {
		t.Fatalf("after a reconcile pass that captured and deleted: %d executors asked for, deleted %v", scoped.passes, scoped.inner.deleted)
	}
	agent.reportChangedStatuses(context.Background())
	if scoped.passes != 2 {
		t.Fatalf("after a status push: %d executors asked for, want 2", scoped.passes)
	}
}

// Actual state that cannot be read ends the pass before it compares anything with desired state.
func TestAPassWhoseExecutorCannotBeBuiltDoesNothing(t *testing.T) {
	server := newUndesiredStateServer(t)
	defer server.Close()
	scoped := &scopedExecutor{inner: &captureExecutor{managed: []string{"exp-gone"}}, err: errors.New("provider unavailable")}
	agent := newTestAgent(server, nil)
	agent.Executor = scoped

	if err := agent.reconcileOnce(context.Background()); !errors.Is(err, scoped.err) {
		t.Fatalf("err = %v", err)
	}
	agent.reportChangedStatuses(context.Background())
	if len(scoped.inner.deleted) != 0 || scoped.inner.created != 0 {
		t.Fatalf("deleted %v, created %d", scoped.inner.deleted, scoped.inner.created)
	}
}
