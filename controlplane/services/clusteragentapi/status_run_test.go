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

const testCadence = 90 * time.Second

// span is what metricsdb.ObserveSpan makes of observations: the gaps of at most cadence, summed.
func span(t *testing.T, observations []metricsdb.Observation) (first, last time.Time, total time.Duration) {
	t.Helper()
	for i, o := range observations {
		if o.ExperimentID != "job-1" || o.ExtraLabels["cluster_name"] != "of-a" || o.ExtraLabels["node"] != "rtx4090-0" {
			t.Fatalf("observation %+v", o)
		}
		if i == 0 {
			first = o.At
			continue
		}
		gap := o.At.Sub(observations[i-1].At)
		if gap < 0 {
			t.Fatalf("observations out of order: %v", observations)
		}
		if gap <= testCadence {
			total += gap
		}
	}
	return first, observations[len(observations)-1].At, total
}

// The run seen on rented machines: 131 s of command that the vendor never showed running, so
// the job was reported pending and then ended. Its two ends alone are further apart than the
// gap rule joins, and the run still counts whole.
func TestARunNeverSeenRunningCountsWhole(t *testing.T) {
	now := time.Date(2026, 10, 6, 21, 43, 30, 0, time.UTC)
	admitted := now.Add(-5 * time.Minute)
	started, ended := now.Add(-150*time.Second), now.Add(-19*time.Second)
	rep := statusReport{ExperimentID: "job-1", Phase: "succeeded", AdmittedNode: "rtx4090-0", StartedAt: &started, EndedAt: &ended}

	first, last, total := span(t, observationsOf(rep, admitted, "of-a", now, testCadence))
	if !first.Equal(started) || !last.Equal(ended) || total != 131*time.Second {
		t.Fatalf("observed %v to %v, %v in all, want the 131 s from %v to %v", first, last, total, started, ended)
	}

	rep.StartedAt, rep.EndedAt = nil, nil
	if got := observationsOf(rep, admitted, "of-a", now, testCadence); len(got) != 0 {
		t.Errorf("an ended job from a runtime that times nothing: %v", got)
	}
	rep.Phase = "running"
	if got := observationsOf(rep, admitted, "of-a", now, testCadence); len(got) != 1 || !got[0].At.Equal(now) {
		t.Errorf("a running job: %v", got)
	}
	rep.Phase, rep.StartedAt, rep.EndedAt = "succeeded", &started, &ended
	if got := observationsOf(rep, time.Time{}, "of-a", now, testCadence); len(got) != 0 {
		t.Errorf("a run of a job that holds no capacity: %v", got)
	}
}

// A rented machine's clock is nobody's to trust. How long the command ran is kept, and when is
// moved into what the platform saw: between the admission and the report.
func TestARunToldByAWrongClockStaysWithinWhatThePlatformSaw(t *testing.T) {
	now := time.Date(2026, 10, 6, 21, 43, 30, 0, time.UTC)
	admitted := now.Add(-5 * time.Minute)
	const ran = 131 * time.Second
	for name, tc := range map[string]struct {
		started             time.Time
		ran                 time.Duration
		wantFirst, wantLast time.Time
	}{
		"right":                        {now.Add(-150 * time.Second), ran, now.Add(-150 * time.Second), now.Add(-19 * time.Second)},
		"an hour ahead":                {now.Add(time.Hour), ran, now.Add(-ran), now},
		"ahead by less than the run":   {now.Add(-time.Minute), ran, now.Add(-ran), now},
		"an hour behind":               {now.Add(-time.Hour), ran, admitted, admitted.Add(ran)},
		"longer than the job was held": {now.Add(-time.Hour), 30 * time.Minute, admitted, now},
	} {
		ended := tc.started.Add(tc.ran)
		rep := statusReport{ExperimentID: "job-1", Phase: "succeeded", AdmittedNode: "rtx4090-0", StartedAt: &tc.started, EndedAt: &ended}
		first, last, total := span(t, observationsOf(rep, admitted, "of-a", now, testCadence))
		if !first.Equal(tc.wantFirst) || !last.Equal(tc.wantLast) || total != tc.wantLast.Sub(tc.wantFirst) {
			t.Errorf("%s: observed %v to %v, %v in all, want %v to %v whole", name, first, last, total, tc.wantFirst, tc.wantLast)
		}
	}
}

func TestAStatusReportNamesBothEndsOfARunInOrderOrNeither(t *testing.T) {
	w := newReconcileWorldWith(t, "of-a", func(base *reconcileStore) Store {
		return placedStore{base, map[string]domain.Placement{"job-1": {ClusterName: "of-a", AdmittedAt: time.Now().Add(-time.Hour)}}}
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
