package metricsdb

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// spanServer replays a raw observation series through the same SQL ObserveSpan issues, so these
// tests pin the interval arithmetic itself rather than a hand-computed expectation of it.
// Timestamps are given as durations before `now`, and may repeat: the two observation series
// routinely carry the same instant, and de-duplication is part of what is being tested.
func spanServer(t *testing.T, now time.Time, gapCap time.Duration, agos []time.Duration) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !strings.HasSuffix(r.URL.Path, "/v1/sql") {
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
			return
		}
		// Distinct, ordered — exactly what the SQL's DISTINCT + ORDER BY produce.
		seen := map[int64]bool{}
		var ts []int64
		for _, ago := range agos {
			ms := now.Add(-ago).UnixMilli()
			if !seen[ms] {
				seen[ms] = true
				ts = append(ts, ms)
			}
		}
		for i := 1; i < len(ts); i++ {
			for j := i; j > 0 && ts[j] < ts[j-1]; j-- {
				ts[j], ts[j-1] = ts[j-1], ts[j]
			}
		}
		if len(ts) == 0 {
			_, _ = w.Write([]byte(`{"output":[{"records":{"rows":[[null,null,null,null]]}}]}`))
			return
		}
		var total, stint, stintStart int64
		for i := 1; i < len(ts); i++ {
			if gap := ts[i] - ts[i-1]; gap > gapCap.Milliseconds() {
				stintStart = ts[i]
			}
		}
		for i := 1; i < len(ts); i++ {
			gap := ts[i] - ts[i-1]
			if gap > gapCap.Milliseconds() {
				continue
			}
			total += gap
			if ts[i] >= stintStart {
				stint += gap
			}
		}
		body, _ := json.Marshal(map[string]any{"output": []any{map[string]any{
			"records": map[string]any{"rows": [][]int64{{ts[0], ts[len(ts)-1], total, stint}}}}}})
		_, _ = w.Write(body)
	}))
}

// A job seen at exactly one instant has been confirmed alive for no measurable time. Duration is
// measured between observations, so one observation yields none — and inventing a reporting
// interval for it would bill time nobody observed.
func TestObservedElapsedChargesNothingForASingleObservation(t *testing.T) {
	step, gapCap := time.Minute, 3*time.Minute
	now := time.Now().UTC()
	server := spanServer(t, now, gapCap, []time.Duration{10 * time.Minute})
	defer server.Close()

	hours, err := ObservedElapsedHoursSince(context.Background(), server.URL, "exp-1", now.Add(-30*step), now, gapCap, step)
	if err != nil {
		t.Fatalf("ObservedElapsedHoursSince: %v", err)
	}
	if hours != 0 {
		t.Fatalf("billed %v hours for a single observation, want 0", hours)
	}
}

// The measurement is the observed span, not a count of grid cells: a job observed across ten
// minutes bills ten minutes whatever the reporting step happens to be.
func TestObservedElapsedSumsTheObservedSpan(t *testing.T) {
	step, gapCap := time.Minute, 3*time.Minute
	now := time.Now().UTC()
	server := spanServer(t, now, gapCap, []time.Duration{20 * time.Minute, 19 * time.Minute, 18 * time.Minute, 17 * time.Minute, 16 * time.Minute, 15 * time.Minute, 14 * time.Minute, 13 * time.Minute, 12 * time.Minute, 11 * time.Minute, 10 * time.Minute})
	defer server.Close()

	hours, err := ObservedElapsedHoursSince(context.Background(), server.URL, "exp-1", now.Add(-30*step), now, gapCap, step)
	if err != nil {
		t.Fatalf("ObservedElapsedHoursSince: %v", err)
	}
	if want := 10 * step.Hours(); !approxHours(hours, want) {
		t.Fatalf("billed %v hours, want %v", hours, want)
	}
}

// The whole reason the old grid was replaced: a job that ran for less than one reporting step
// quantised to zero and was billed nothing at all.
func TestObservedElapsedBillsAJobShorterThanOneStep(t *testing.T) {
	step, gapCap := time.Minute, 3*time.Minute
	now := time.Now().UTC()
	// Observed from t-30s to t-5s: 25 seconds, well under the 60s step.
	server := spanServer(t, now, gapCap, []time.Duration{30 * time.Second, 20 * time.Second, 12 * time.Second, 5 * time.Second})
	defer server.Close()

	hours, err := ObservedElapsedHoursSince(context.Background(), server.URL, "exp-1", now.Add(-30*step), now, gapCap, step)
	if err != nil {
		t.Fatalf("ObservedElapsedHoursSince: %v", err)
	}
	if want := 25 * time.Second.Hours(); !approxHours(hours, want) {
		t.Fatalf("billed %v hours for a 25s job, want %v", hours, want)
	}
}

// A gap longer than the cap is time the job was genuinely not running (preempted, between pods,
// node dead) and contributes nothing — while a gap at exactly the cap is still ordinary jitter.
func TestObservedElapsedSkipsGapsPastTheCapButNotAtIt(t *testing.T) {
	step, gapCap := time.Minute, 3*time.Minute
	now := time.Now().UTC()

	// Two stints of 2 minutes each, separated by a 5-minute gap.
	server := spanServer(t, now, gapCap, []time.Duration{20 * time.Minute, 19 * time.Minute, 18 * time.Minute, 13 * time.Minute, 12 * time.Minute, 11 * time.Minute})
	defer server.Close()
	hours, err := ObservedElapsedHoursSince(context.Background(), server.URL, "exp-1", now.Add(-30*step), now, gapCap, step)
	if err != nil {
		t.Fatalf("ObservedElapsedHoursSince: %v", err)
	}
	if want := 4 * step.Hours(); !approxHours(hours, want) {
		t.Fatalf("billed %v hours across a 5-minute outage, want %v (the gap itself is not billable)", hours, want)
	}

	// The same shape with the gap exactly at the cap counts in full: 3 minutes is jitter, not an outage.
	atCap := spanServer(t, now, gapCap, []time.Duration{20 * time.Minute, 19 * time.Minute, 18 * time.Minute, 15 * time.Minute, 14 * time.Minute, 13 * time.Minute})
	defer atCap.Close()
	hours, err = ObservedElapsedHoursSince(context.Background(), atCap.URL, "exp-1", now.Add(-30*step), now, gapCap, step)
	if err != nil {
		t.Fatalf("ObservedElapsedHoursSince: %v", err)
	}
	if want := 7 * step.Hours(); !approxHours(hours, want) {
		t.Fatalf("billed %v hours with a gap exactly at the cap, want %v", hours, want)
	}
}

// The two observation series routinely record the same instant. Counting it twice would make
// multiplicity part of the bill.
func TestObservedElapsedIgnoresDuplicateTimestamps(t *testing.T) {
	step, gapCap := time.Minute, 3*time.Minute
	now := time.Now().UTC()
	server := spanServer(t, now, gapCap, []time.Duration{20 * time.Minute, 20 * time.Minute, 19 * time.Minute, 19 * time.Minute, 18 * time.Minute, 18 * time.Minute})
	defer server.Close()

	hours, err := ObservedElapsedHoursSince(context.Background(), server.URL, "exp-1", now.Add(-30*step), now, gapCap, step)
	if err != nil {
		t.Fatalf("ObservedElapsedHoursSince: %v", err)
	}
	if want := 2 * step.Hours(); !approxHours(hours, want) {
		t.Fatalf("billed %v hours, want %v", hours, want)
	}
}

// A preemption rescale must subtract only what the job has accrued since it last resumed, or an
// earlier stint is charged against the shortened estimate a second time.
func TestObservedStintCountsOnlyTheRunSinceTheLastOutage(t *testing.T) {
	step, gapCap := time.Minute, 3*time.Minute
	now := time.Now().UTC()
	// 2 minutes, a 5-minute outage, then 3 minutes.
	server := spanServer(t, now, gapCap, []time.Duration{20 * time.Minute, 19 * time.Minute, 18 * time.Minute, 13 * time.Minute, 12 * time.Minute, 11 * time.Minute, 10 * time.Minute})
	defer server.Close()

	hours, err := ObservedStintElapsedHours(context.Background(), server.URL, "exp-1", now, 30*step, gapCap, step)
	if err != nil {
		t.Fatalf("ObservedStintElapsedHours: %v", err)
	}
	if want := 3 * step.Hours(); !approxHours(hours, want) {
		t.Fatalf("stint billed %v hours, want %v (only the run since the outage)", hours, want)
	}
}

// Settling promptly and settling long afterwards have to produce the same number: the samples are
// immutable, and nothing about the answer may depend on when it is asked.
func TestObservedElapsedIsStableHoweverLateItIsAsked(t *testing.T) {
	step, gapCap := time.Minute, 3*time.Minute
	now := time.Now().UTC()
	offsets := []time.Duration{20 * time.Minute, 19 * time.Minute, 18 * time.Minute, 17 * time.Minute, 16 * time.Minute}

	prompt := spanServer(t, now, gapCap, offsets)
	defer prompt.Close()
	early, err := ObservedElapsedHoursSince(context.Background(), prompt.URL, "exp-1", now.Add(-30*step), now, gapCap, step)
	if err != nil {
		t.Fatalf("ObservedElapsedHoursSince: %v", err)
	}

	// The same series read an hour later: every offset is one hour further back.
	later := now.Add(time.Hour)
	shifted := make([]time.Duration, len(offsets))
	for i, o := range offsets {
		shifted[i] = o + time.Hour
	}
	lateSrv := spanServer(t, later, gapCap, shifted)
	defer lateSrv.Close()
	late, err := ObservedElapsedHoursSince(context.Background(), lateSrv.URL, "exp-1", later.Add(-90*step), later, gapCap, step)
	if err != nil {
		t.Fatalf("ObservedElapsedHoursSince: %v", err)
	}
	if !approxHours(early, late) {
		t.Fatalf("same job measured %v hours promptly and %v hours an hour later", early, late)
	}
}

// The measurement runs per running job on every quota read, so its round-trip count is a property
// worth pinning. One query answers first-observation, total elapsed and current stint together.
func TestObservedElapsedMakesOneRoundTrip(t *testing.T) {
	step, gapCap := time.Minute, 3*time.Minute
	now := time.Now().UTC()

	var queries int
	inner := spanServer(t, now, gapCap, []time.Duration{20 * time.Minute, 15 * time.Minute, 10 * time.Minute})
	defer inner.Close()
	counting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries++
		inner.Config.Handler.ServeHTTP(w, r)
	}))
	defer counting.Close()

	if _, _, err := ObservedElapsed(context.Background(), counting.URL, "exp-1", now, 30*step, gapCap, step); err != nil {
		t.Fatalf("ObservedElapsed: %v", err)
	}
	if queries != 1 {
		t.Fatalf("made %d metrics queries, want 1", queries)
	}
}

func approxHours(got, want float64) bool {
	d := got - want
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}
