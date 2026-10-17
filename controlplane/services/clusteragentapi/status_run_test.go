package clusteragentapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/metricsdb"
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
	rep := clusterapi.StatusReport{ExperimentID: "job-1", Phase: "succeeded", AdmittedNode: "rtx4090-0", StartedAt: &started, EndedAt: &ended}

	first, last, total := span(t, observationsOf(rep, admitted, now, "of-a", now, testCadence))
	if !first.Equal(started) || !last.Equal(ended) || total != 131*time.Second {
		t.Fatalf("observed %v to %v, %v in all, want the 131 s from %v to %v", first, last, total, started, ended)
	}

	rep.StartedAt, rep.EndedAt = nil, nil
	if got := observationsOf(rep, admitted, time.Time{}, "of-a", now, testCadence); len(got) != 0 {
		t.Errorf("an ended job from a runtime that times nothing: %v", got)
	}
	rep.Phase = "running"
	if got := observationsOf(rep, admitted, time.Time{}, "of-a", now, testCadence); len(got) != 1 || !got[0].At.Equal(now) {
		t.Errorf("a running job: %v", got)
	}
	rep.Phase, rep.StartedAt, rep.EndedAt = "succeeded", &started, &ended
	for name, at := range map[string][2]time.Time{"holds no capacity": {{}, now}, "has no first report": {admitted, {}}} {
		if got := observationsOf(rep, at[0], at[1], "of-a", now, testCadence); len(got) != 0 {
			t.Errorf("a run of a job that %s: %v", name, got)
		}
	}
}

// A runtime reports an ended run on every push until its workload is deleted. From a machine
// whose clock is ahead the run ends at its report, and placed by the time of each push it would
// be observed for longer with every one. Placed by the first report, the pushes after it add
// nothing.
func TestARunReportedAgainIsCountedOnce(t *testing.T) {
	reported := time.Date(2026, 10, 6, 21, 43, 30, 0, time.UTC)
	admitted := reported.Add(-5 * time.Minute)
	const ran = 131 * time.Second
	started := reported.Add(time.Hour)
	ended := started.Add(ran)
	rep := clusterapi.StatusReport{ExperimentID: "job-1", Phase: "succeeded", AdmittedNode: "rtx4090-0", StartedAt: &started, EndedAt: &ended}

	seen := map[time.Time]bool{}
	var union []metricsdb.Observation
	for _, pushedAfter := range []time.Duration{0, 5 * time.Second, 40 * time.Second, 10 * time.Minute} {
		for _, o := range observationsOf(rep, admitted, reported, "of-a", reported.Add(pushedAfter), testCadence) {
			if !seen[o.At] {
				seen[o.At] = true
				union = append(union, o)
			}
		}
	}
	slices.SortFunc(union, func(a, b metricsdb.Observation) int { return a.At.Compare(b.At) })
	first, last, total := span(t, union)
	if !first.Equal(reported.Add(-ran)) || !last.Equal(reported) || total != ran {
		t.Fatalf("four pushes of one run are observed %v to %v, %v in all, want the %v that end at the first report", first, last, total, ran)
	}
}

// The first report's time is the store's, asked for the jobs whose report carries an end and for
// no other, and a push that cannot learn it records nothing.
func TestAStatusPushPlacesAnEndedRunByItsFirstReport(t *testing.T) {
	admitted := time.Now().Add(-time.Hour)
	var failing error
	w := newReconcileWorldWith(t, "of-a", func(base *reconcileStore) Store {
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

// A rented machine's clock is nobody's to trust. How long the command ran is kept, and when is
// moved into what the platform saw: between the admission and the first report.
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
		rep := clusterapi.StatusReport{ExperimentID: "job-1", Phase: "succeeded", AdmittedNode: "rtx4090-0", StartedAt: &tc.started, EndedAt: &ended}
		first, last, total := span(t, observationsOf(rep, admitted, now, "of-a", now, testCadence))
		if !first.Equal(tc.wantFirst) || !last.Equal(tc.wantLast) || total != tc.wantLast.Sub(tc.wantFirst) {
			t.Errorf("%s: observed %v to %v, %v in all, want %v to %v whole", name, first, last, total, tc.wantFirst, tc.wantLast)
		}
	}
}

func TestAStatusReportNamesBothEndsOfARunInOrderOrNeither(t *testing.T) {
	w := newReconcileWorldWith(t, "of-a", func(base *reconcileStore) Store {
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
