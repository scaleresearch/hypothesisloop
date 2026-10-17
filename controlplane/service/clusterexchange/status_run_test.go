package clusterexchange

import (
	"slices"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
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
