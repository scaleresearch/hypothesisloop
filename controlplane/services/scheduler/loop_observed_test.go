package scheduler

import (
	"context"
	"testing"
	"time"
)

// fakeObserved answers the loop's observed-state reads from plain maps. It replaced an httptest
// server returning hand-written GreptimeDB result JSON: that stub could only ever say one thing,
// so preempt()'s victim ranking — which is entirely a function of what observed time each
// candidate reports — had no way to be tested at all.
//
// An experiment missing from a map reads as "nothing observed yet", which is the real store's
// answer for a job that has not reported a metric, not an error.
type fakeObserved struct {
	elapsed map[string]float64
	stint   map[string]float64
	node    map[string]string
	// err, when set, fails every read — the loop must survive a metrics store that is down.
	err error
}

func (f fakeObserved) ObservedElapsedHours(_ context.Context, experimentID string, _, _ time.Time) (float64, error) {
	if f.err != nil {
		return 0, f.err
	}
	return f.elapsed[experimentID], nil
}

func (f fakeObserved) ObservedStintElapsedHours(_ context.Context, experimentID string, _, _ time.Time) (float64, error) {
	if f.err != nil {
		return 0, f.err
	}
	return f.stint[experimentID], nil
}

func (f fakeObserved) LatestExperimentNode(_ context.Context, experimentID string, _, _ time.Time) (string, bool, error) {
	if f.err != nil {
		return "", false, f.err
	}
	node, found := f.node[experimentID]
	return node, found, nil
}

// observedOnNode is the common case: every experiment is attributed to the same node and nothing
// else about observed state matters to the test.
func observedOnNode(ids []string, node string) fakeObserved {
	byID := make(map[string]string, len(ids))
	for _, id := range ids {
		byID[id] = node
	}
	return fakeObserved{node: byID}
}

// TestFakeObservedMatchesTheInterface keeps the fake honest: if ObservedState gains a method, this
// stops compiling here rather than in whichever test happens to be edited next.
func TestFakeObservedMatchesTheInterface(t *testing.T) {
	var _ ObservedState = fakeObserved{}
	var _ ObservedState = metricsDBObserver{}
}
