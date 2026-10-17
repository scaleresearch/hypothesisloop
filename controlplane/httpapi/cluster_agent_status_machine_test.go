package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/clusterexchange"
)

type placedStore struct {
	*reconcileStore
	placements map[string]domain.Placement
}

func (s placedStore) PlacementByID(context.Context, []string) (map[string]domain.Placement, error) {
	return s.placements, nil
}

// The machine behind the node was added to a report that runtimes already in the field send
// without it, and a report naming a field the body does not know is refused. So the report of a
// runtime built before the field stays valid, and the field is taken only beside the node it
// describes.
func TestAStatusReportMayNameTheMachineBehindItsNodeAndNeedNot(t *testing.T) {
	w := newReconcileWorldWith(t, "of-a", func(base *reconcileStore) clusterexchange.Store {
		return placedStore{base, map[string]domain.Placement{"job-1": {ClusterName: "of-a"}}}
	})
	push := func(report map[string]any) int {
		t.Helper()
		body, err := json.Marshal(map[string]any{"reports": []any{report}})
		if err != nil {
			t.Fatal(err)
		}
		req := authtest.Authorize(httptest.NewRequest("POST", "/of-a/status", bytes.NewReader(body)))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		w.router.ServeHTTP(rec, req)
		return rec.Code
	}
	machine := map[string]any{"instance_id": "54479019", "host_id": "11230", "accelerator_model": "RTX 4090", "price_per_hour_usd": 0.4544}

	if code := push(map[string]any{"experiment_id": "job-1", "phase": "running", "admitted_node": "node-a"}); code != 200 {
		t.Errorf("a report from a runtime built before the field = %d, want 200", code)
	}
	if code := push(map[string]any{"experiment_id": "job-1", "phase": "running", "admitted_node": "rtx4090-0", "admitted_machine": machine}); code != 200 {
		t.Errorf("a report naming the machine behind its node = %d, want 200", code)
	}
	if code := push(map[string]any{"experiment_id": "job-1", "phase": "running", "admitted_node": "rtx4090-0", "admitted_machine": map[string]any{}}); code != 200 {
		t.Errorf("a report from a runtime that knows nothing of the machine = %d, want 200", code)
	}
	if code := push(map[string]any{"experiment_id": "job-1", "phase": "running", "admitted_machine": machine}); code != 400 {
		t.Errorf("a machine with no node = %d, want 400", code)
	}
	if code := push(map[string]any{"experiment_id": "job-1", "phase": "running", "admitted_node": "rtx4090-0", "admitted_machine": map[string]any{"gpu_name": "RTX 4090"}}); code != 422 {
		t.Errorf("a machine described in a vendor's field = %d, want 422", code)
	}
}

// The phase that ends a job is what its workload is deleted on. A push whose log tail or phase
// detail cannot be written records no phase and is refused, so the runtime pushes it again and
// the reason the job ended is not lost.
func TestAStatusPushWhoseLogTailOrPhaseDetailCannotBeWrittenIsRefused(t *testing.T) {
	w := newReconcileWorldWith(t, "of-a", func(base *reconcileStore) clusterexchange.Store {
		return placedStore{base, map[string]domain.Placement{"job-1": {ClusterName: "of-a"}}}
	})
	push := func(extra map[string]any) int {
		t.Helper()
		report := map[string]any{"experiment_id": "job-1", "phase": "failed"}
		for k, v := range extra {
			report[k] = v
		}
		body, err := json.Marshal(map[string]any{"reports": []any{report}})
		if err != nil {
			t.Fatal(err)
		}
		req := authtest.Authorize(httptest.NewRequest("POST", "/of-a/status", bytes.NewReader(body)))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		w.router.ServeHTTP(rec, req)
		return rec.Code
	}
	for name, extra := range map[string]map[string]any{
		"a log tail":     {"log_tail": []string{"Traceback (most recent call last):"}},
		"a phase detail": {"reason": domain.PhaseReasonContainerFailed, "message": "exit code 1"},
	} {
		w.sqlDown.Store(false)
		writes := w.metricsWrites.Load()
		if code := push(extra); code != 200 || w.metricsWrites.Load() != writes+1 {
			t.Errorf("%s the store takes = %d with %d phases recorded, want 200 and one", name, code, w.metricsWrites.Load()-writes)
		}
		w.sqlDown.Store(true)
		writes = w.metricsWrites.Load()
		if code := push(extra); code != 500 || w.metricsWrites.Load() != writes {
			t.Errorf("%s the store refuses = %d with %d phases recorded, want 500 and none", name, code, w.metricsWrites.Load()-writes)
		}
	}
}
