package metricsdb

import (
	"strconv"
	"testing"
	"time"
)

// A zero-value start time (a row written before CreatedAt was populated — this actually happened
// in dev data) used to produce a ~2000-year duration literal. GreptimeDB's PromQL parser rejects
// that outright ("duration must be greater than 0"), 500ing every quota read for the experiment
// forever. ObservedLookback must ceiling the window instead of forwarding whatever
// time.Since(start) happens to compute.
func TestObservedLookbackCeilingsAZeroStartTime(t *testing.T) {
	got := ObservedLookback(time.Time{})
	want := ObservedLookback(time.Now().Add(-maxObservedLookback))
	if got != want {
		t.Errorf("ObservedLookback(zero) = %q, want the ceiling %q", got, want)
	}
	seconds := parseSecondsSuffix(t, got)
	if d := time.Duration(seconds) * time.Second; d > maxObservedLookback {
		t.Errorf("ObservedLookback(zero) = %s, want <= maxObservedLookback (%s)", got, maxObservedLookback)
	}
}

func TestObservedLookbackFloorsAFutureStartTime(t *testing.T) {
	got := ObservedLookback(time.Now().Add(time.Minute))
	seconds := parseSecondsSuffix(t, got)
	if d := time.Duration(seconds) * time.Second; d < minObservedLookback {
		t.Errorf("ObservedLookback(future) = %s, want >= minObservedLookback (%s)", got, minObservedLookback)
	}
}

func TestObservedLookbackCoversARealMultiWeekExperiment(t *testing.T) {
	got := ObservedLookback(time.Now().Add(-14 * 24 * time.Hour))
	seconds := parseSecondsSuffix(t, got)
	d := time.Duration(seconds) * time.Second
	if d < 13*24*time.Hour || d > maxObservedLookback {
		t.Errorf("ObservedLookback(14 days ago) = %s, want ~14 days, uncapped", got)
	}
}

func parseSecondsSuffix(t *testing.T, s string) int64 {
	t.Helper()
	if len(s) < 2 || s[len(s)-1] != 's' {
		t.Fatalf("ObservedLookback returned %q, want a \"<N>s\" string", s)
	}
	n, err := strconv.ParseInt(s[:len(s)-1], 10, 64)
	if err != nil {
		t.Fatalf("ObservedLookback returned %q, want a parseable integer before the trailing s: %v", s, err)
	}
	return n
}
