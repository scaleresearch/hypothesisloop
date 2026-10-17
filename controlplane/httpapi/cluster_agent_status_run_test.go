package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/clusterexchange"
)

// The first report's time is the store's, asked for the jobs whose report carries an end and for
// no other, and a push that cannot learn it records nothing.
func TestAStatusPushPlacesAnEndedRunByItsFirstReport(t *testing.T) {
	admitted := time.Now().Add(-time.Hour)
	var failing error
	w := newReconcileWorldWith(t, "of-a", func(base *reconcileStore) clusterexchange.Store {
		return failingRunStore{placedStore{base, map[string]domain.Placement{
			"job-ended": {ClusterName: "of-a", AdmittedAt: admitted}, "job-running": {ClusterName: "of-a", AdmittedAt: admitted},
			"job-elsewhere": {ClusterName: "of-b", AdmittedAt: admitted},
		}}, &failing}
	})
	w.store.runReportedAt = map[string]time.Time{"job-ended": admitted.Add(10 * time.Minute)}
	run := map[string]any{"phase": "succeeded", "started_at": "2026-10-06T21:42:50Z", "ended_at": "2026-10-06T21:43:05Z"}
	push := func() int {
		t.Helper()
		reports := []any{map[string]any{"experiment_id": "job-running", "phase": "running"}}
		for _, id := range []string{"job-ended", "job-elsewhere"} {
			report := map[string]any{"experiment_id": id}
			for k, v := range run {
				report[k] = v
			}
			reports = append(reports, report)
		}
		body, err := json.Marshal(map[string]any{"reports": reports})
		if err != nil {
			t.Fatal(err)
		}
		req := authtest.Authorize(httptest.NewRequest("POST", "/of-a/status", bytes.NewReader(body)))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		w.router.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := push(); code != 200 {
		t.Fatalf("push = %d, want 200", code)
	}
	if want := [][]string{{"job-ended"}}; !reflect.DeepEqual(w.store.runsAsked, want) {
		t.Errorf("asked the first report of %v, want %v", w.store.runsAsked, want)
	}

	failing = errors.New("postgres is down")
	writes := w.metricsWrites.Load()
	if code := push(); code != 500 || w.metricsWrites.Load() != writes {
		t.Errorf("a push whose first report cannot be read = %d with %d writes, want 500 and nothing recorded", code, w.metricsWrites.Load()-writes)
	}
}

type failingRunStore struct {
	placedStore
	err *error
}

func (s failingRunStore) RunFirstReportedAt(ctx context.Context, ids []string) (map[string]time.Time, error) {
	if *s.err != nil {
		return nil, *s.err
	}
	return s.placedStore.RunFirstReportedAt(ctx, ids)
}

func TestAStatusReportNamesBothEndsOfARunInOrderOrNeither(t *testing.T) {
	w := newReconcileWorldWith(t, "of-a", func(base *reconcileStore) clusterexchange.Store {
		return placedStore{base, map[string]domain.Placement{"job-1": {ClusterName: "of-a", AdmittedAt: time.Now().Add(-time.Hour)}}}
	})
	w.store.runReportedAt = map[string]time.Time{"job-1": time.Now()}
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
