package clusteragentapi

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
)

// A job that started and ended between two reports was never reported running, and is measured
// by the two ends of its run alone.
func TestTheRunARuntimeMeasuredIsObservedAtItsTwoEnds(t *testing.T) {
	now := time.Date(2026, 10, 6, 21, 43, 30, 0, time.UTC)
	started, ended := now.Add(-40*time.Second), now.Add(-25*time.Second)
	times := func(observations []metricsdb.Observation) []time.Time {
		var at []time.Time
		for _, o := range observations {
			if o.ExperimentID != "job-1" || o.ExtraLabels["cluster_name"] != "of-a" || o.ExtraLabels["node"] != "rtx4090-0" {
				t.Fatalf("observation %+v", o)
			}
			at = append(at, o.At)
		}
		return at
	}
	equal := func(got []time.Time, want ...time.Time) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range want {
			if !got[i].Equal(want[i]) {
				return false
			}
		}
		return true
	}
	rep := statusReport{ExperimentID: "job-1", Phase: "succeeded", AdmittedNode: "rtx4090-0", StartedAt: &started, EndedAt: &ended}

	if got, ahead := observationsOf(rep, "of-a", now); ahead || !equal(times(got), started, ended) {
		t.Errorf("an ended job with its run: %v, ahead %v", got, ahead)
	}
	rep.StartedAt, rep.EndedAt = nil, nil
	if got, ahead := observationsOf(rep, "of-a", now); ahead || len(got) != 0 {
		t.Errorf("an ended job from a runtime that times nothing: %v, ahead %v", got, ahead)
	}
	rep.Phase = "running"
	if got, ahead := observationsOf(rep, "of-a", now); ahead || !equal(times(got), now) {
		t.Errorf("a running job: %v, ahead %v", got, ahead)
	}
	rep.Phase = "succeeded"
	skewedStart, skewedEnd := now.Add(time.Hour), now.Add(time.Hour+15*time.Second)
	rep.StartedAt, rep.EndedAt = &skewedStart, &skewedEnd
	if got, ahead := observationsOf(rep, "of-a", now); !ahead || len(got) != 0 {
		t.Errorf("a run that ends an hour from now: %v, ahead %v", got, ahead)
	}
}

func TestAStatusReportNamesBothEndsOfARunInOrderOrNeither(t *testing.T) {
	w := newReconcileWorldWith(t, "of-a", func(base *reconcileStore) Store {
		return placedStore{base, map[string]domain.Placement{"job-1": {ClusterName: "of-a"}}}
	})
	push := func(extra map[string]any) int {
		t.Helper()
		report := map[string]any{"experiment_id": "job-1", "phase": "succeeded", "admitted_node": "rtx4090-0"}
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
	const started, ended = "2026-10-06T21:42:50.123Z", "2026-10-06T21:43:05.456Z"

	writes := w.metricsWrites.Load()
	if code := push(nil); code != 200 || w.metricsWrites.Load() != writes+1 {
		t.Errorf("no run = %d with %d writes, want 200 and the statuses alone", code, w.metricsWrites.Load()-writes)
	}
	writes = w.metricsWrites.Load()
	if code := push(map[string]any{"started_at": started, "ended_at": ended}); code != 200 || w.metricsWrites.Load() != writes+2 {
		t.Errorf("a run = %d with %d writes, want 200 and its observations beside the statuses", code, w.metricsWrites.Load()-writes)
	}
	for name, extra := range map[string]map[string]any{
		"a start alone": {"started_at": started},
		"an end alone":  {"ended_at": ended},
		"an end first":  {"started_at": ended, "ended_at": started},
	} {
		if code := push(extra); code != 400 {
			t.Errorf("%s = %d, want 400", name, code)
		}
	}
}
