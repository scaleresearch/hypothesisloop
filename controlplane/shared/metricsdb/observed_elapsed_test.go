package metricsdb

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// gridServer answers every range query with samples at the given offsets before `now`, and every
// instant query with an empty vector. Offsets are in units of `step`, newest last.
//
// The gap cap is applied by GreptimeDB in production (last_over_time's range window), so a test
// server that replays exactly the samples it was given is modelling the *raw* series. To model
// the carry-forward too, callers pass the offsets they expect the query to see.
func gridServer(t *testing.T, now time.Time, step time.Duration, rangeOffsets, sampleOffsets []int) *httptest.Server {
	t.Helper()
	matrix := func(offsets []int) string {
		var values []string
		for _, o := range offsets {
			values = append(values, fmt.Sprintf(`[%d,"1"]`, now.Add(-time.Duration(o)*step).Unix()))
		}
		if len(values) == 0 {
			return `{"status":"success","data":{"resultType":"matrix","result":[]}}`
		}
		return fmt.Sprintf(`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[%s]}]}}`,
			strings.Join(values, ","))
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		// sampleBounds asks with a range equal to step; the alive grid asks with gapCap.
		// Telling them apart by the query string is what lets one server model both.
		if strings.Contains(r.Form.Get("query"), fmt.Sprintf("[%ds]", int(step.Seconds()))) {
			_, _ = w.Write([]byte(matrix(sampleOffsets)))
			return
		}
		_, _ = w.Write([]byte(matrix(rangeOffsets)))
	}))
}

// Samples are boundaries, not durations. A job seen at exactly one instant has been confirmed
// alive for no measurable time, and must not be billed a full reporting interval for it.
func TestObservedElapsedChargesNothingForASingleObservation(t *testing.T) {
	step := time.Minute
	now := time.Now().UTC()
	server := gridServer(t, now, step, []int{10}, []int{10})
	defer server.Close()

	hours, err := ObservedElapsedHoursSince(context.Background(), server.URL, "exp-1", now.Add(-30*step), now, 3*step, step)
	if err != nil {
		t.Fatalf("ObservedElapsedHoursSince: %v", err)
	}
	if hours != 0 {
		t.Fatalf("billed %v hours for a single observation, want 0", hours)
	}
}

// n boundaries delimit n-1 intervals. Eleven samples one minute apart is ten minutes of
// confirmed-alive time, not eleven.
func TestObservedElapsedCountsIntervalsNotPoints(t *testing.T) {
	step := time.Minute
	now := time.Now().UTC()
	offsets := []int{20, 19, 18, 17, 16, 15, 14, 13, 12, 11, 10}
	server := gridServer(t, now, step, offsets, offsets)
	defer server.Close()

	hours, err := ObservedElapsedHoursSince(context.Background(), server.URL, "exp-1", now.Add(-30*step), now, 3*step, step)
	if err != nil {
		t.Fatalf("ObservedElapsedHoursSince: %v", err)
	}
	if want := 10 * step.Hours(); !approxHours(hours, want) {
		t.Fatalf("billed %v hours, want %v (11 boundaries = 10 intervals)", hours, want)
	}
}

// The bill must not depend on how long after the job died the query happens to run. The gap cap
// keeps producing grid points after the last real sample — right for a mid-run blip, but past the
// end there is nothing to bridge to, so those points are time nobody observed.
func TestObservedElapsedDoesNotBillTheGapCapPastTheLastSample(t *testing.T) {
	step := time.Minute
	now := time.Now().UTC()
	// Real samples stop at t-10. last_over_time carries them forward for gapCap (3 steps).
	realSamples := []int{20, 19, 18, 17, 16, 15, 14, 13, 12, 11, 10}
	carriedForward := append(append([]int{}, realSamples...), 9, 8, 7)
	server := gridServer(t, now, step, carriedForward, realSamples)
	defer server.Close()

	hours, err := ObservedElapsedHoursSince(context.Background(), server.URL, "exp-1", now.Add(-30*step), now, 3*step, step)
	if err != nil {
		t.Fatalf("ObservedElapsedHoursSince: %v", err)
	}
	// Same answer as the run without carry-forward: the extrapolated tail is not billable.
	if want := 10 * step.Hours(); !approxHours(hours, want) {
		t.Fatalf("billed %v hours, want %v — the gap-cap tail past the last real sample was billed", hours, want)
	}
}

// A window containing no real sample at all — only points last_over_time carried forward from a
// sample that landed before the window opened — is billed as zero. Every point in it is
// extrapolation, and the rule that trailing carry-forward is not billable does not stop applying
// just because the last real sample fell outside the range being asked about. Counting the union
// here instead would make a job that died before `since` bill up to a full gapCap, and bill it
// differently depending on when the query ran.
func TestObservedElapsedBillsNothingWhenTheWindowHoldsOnlyCarryForward(t *testing.T) {
	step := time.Minute
	now := time.Now().UTC()
	// The alive grid has points (carried forward); the real-sample grid has none.
	server := gridServer(t, now, step, []int{5, 4, 3}, nil)
	defer server.Close()

	hours, err := ObservedElapsedHoursSince(context.Background(), server.URL, "exp-1", now.Add(-10*step), now, 3*step, step)
	if err != nil {
		t.Fatalf("ObservedElapsedHoursSince: %v", err)
	}
	if hours != 0 {
		t.Fatalf("billed %v hours for a window of pure carry-forward, want 0", hours)
	}
}

// The billing measurement runs per running job on every quota read, so its round-trip count is a
// property worth pinning, not an incidental. Two series (heartbeat, training metric) times two
// distinct range selectors (step for real samples, gapCap for the alive grid) is four — anything
// more means the same series is being fetched twice with the same parameters.
func TestObservedElapsedMakesFourRoundTrips(t *testing.T) {
	step := time.Minute
	now := time.Now().UTC()
	offsets := []int{20, 15, 10}

	var queries int
	inner := gridServer(t, now, step, offsets, offsets)
	defer inner.Close()
	counting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries++
		inner.Config.Handler.ServeHTTP(w, r)
	}))
	defer counting.Close()

	if _, _, err := ObservedElapsed(context.Background(), counting.URL, "exp-1", now, 30*step, 3*step, step); err != nil {
		t.Fatalf("ObservedElapsed: %v", err)
	}
	if queries != 4 {
		t.Fatalf("made %d metrics queries, want 4", queries)
	}
}

func approxHours(got, want float64) bool {
	d := got - want
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}
