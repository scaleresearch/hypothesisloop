package agentloop

import (
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

func exps(ids ...string) []*domain.Experiment {
	out := make([]*domain.Experiment, 0, len(ids))
	for _, id := range ids {
		out = append(out, &domain.Experiment{ID: id})
	}
	return out
}

func TestIndexDesired(t *testing.T) {
	for _, tc := range []struct {
		name         string
		in           []*domain.Experiment
		wantIDs      []string
		wantComplete bool
		wantErrs     int
	}{
		{name: "nothing desired is a complete answer, not a missing one",
			in: nil, wantIDs: nil, wantComplete: true},
		{name: "ordinary rows",
			in: exps("a", "b"), wantIDs: []string{"a", "b"}, wantComplete: true},
		{name: "an identity-less row is dropped and costs completeness",
			in: exps("a", ""), wantIDs: []string{"a"}, wantComplete: false, wantErrs: 1},
		{name: "a nil row is treated exactly like an identity-less one",
			in: append(exps("a"), nil), wantIDs: []string{"a"}, wantComplete: false, wantErrs: 1},
		{name: "a duplicated id is quarantined, not resolved in favour of either copy",
			in: exps("a", "dup", "dup"), wantIDs: []string{"a"}, wantComplete: false, wantErrs: 1},
		// The case that made quarantining by map-deletion alone wrong: the third copy finds the
		// id absent and would reinstate it as though it had never been ambiguous.
		{name: "a thrice-duplicated id stays quarantined",
			in: exps("dup", "dup", "dup"), wantIDs: nil, wantComplete: false, wantErrs: 1},
		{name: "a four-times-duplicated id is still reported exactly once",
			in: exps("dup", "dup", "dup", "dup"), wantIDs: nil, wantComplete: false, wantErrs: 1},
		{name: "two different duplicated ids are each quarantined and each reported",
			in: exps("x", "x", "y", "y", "keep"), wantIDs: []string{"keep"}, wantComplete: false, wantErrs: 2},
		{name: "a duplicate appearing after other rows does not evict them",
			in: exps("a", "b", "a"), wantIDs: []string{"b"}, wantComplete: false, wantErrs: 1},
		{name: "every kind of bad row at once",
			in: append(exps("good", "", "dup", "dup"), nil), wantIDs: []string{"good"}, wantComplete: false, wantErrs: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := indexDesired(tc.in)
			if len(got.byID) != len(tc.wantIDs) {
				t.Fatalf("indexed %d id(s) %v, want %d %v", len(got.byID), keysOf(got.byID), len(tc.wantIDs), tc.wantIDs)
			}
			for _, id := range tc.wantIDs {
				if got.byID[id] == nil {
					t.Errorf("id %q missing from the index", id)
				}
			}
			if got.complete != tc.wantComplete {
				t.Errorf("complete = %v, want %v — this is what decides whether the pass may delete on absence", got.complete, tc.wantComplete)
			}
			if len(got.errs) != tc.wantErrs {
				t.Errorf("got %d error(s) %v, want %d", len(got.errs), got.errs, tc.wantErrs)
			}
		})
	}
}

func TestIndexActual(t *testing.T) {
	for _, tc := range []struct {
		name         string
		in           []string
		wantIDs      []string
		wantComplete bool
		wantErrs     int
	}{
		{name: "nothing running is a complete answer",
			in: nil, wantIDs: nil, wantComplete: true},
		{name: "ordinary ids",
			in: []string{"a", "b"}, wantIDs: []string{"a", "b"}, wantComplete: true},
		{name: "an unnamed workload costs completeness, because creation argues from absence",
			in: []string{"a", ""}, wantIDs: []string{"a"}, wantComplete: false, wantErrs: 1},
		// Unlike the desired side: a delete removes every workload carrying the identity, so the
		// copy is already accounted for and the snapshot is still complete.
		{name: "a duplicate id is reported but does not cost completeness",
			in: []string{"a", "a"}, wantIDs: []string{"a"}, wantComplete: true, wantErrs: 1},
		{name: "several unnamed workloads are each reported",
			in: []string{"", "", "a"}, wantIDs: []string{"a"}, wantComplete: false, wantErrs: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := indexActual(tc.in)
			if len(got.ids) != len(tc.wantIDs) {
				t.Fatalf("indexed %d id(s), want %d %v", len(got.ids), len(tc.wantIDs), tc.wantIDs)
			}
			for _, id := range tc.wantIDs {
				if !got.ids[id] {
					t.Errorf("id %q missing from the set", id)
				}
			}
			if got.complete != tc.wantComplete {
				t.Errorf("complete = %v, want %v — this is what decides whether the pass may create on absence", got.complete, tc.wantComplete)
			}
			if len(got.errs) != tc.wantErrs {
				t.Errorf("got %d error(s) %v, want %d", len(got.errs), got.errs, tc.wantErrs)
			}
		})
	}
}

// The errors are what a human reads when a feed goes wrong, so they have to name the id they are
// about — "duplicate experiment" with no id sends someone to read the whole feed by hand.
func TestSnapshotErrorsNameTheOffendingID(t *testing.T) {
	got := indexDesired(exps("dup", "dup"))
	if len(got.errs) != 1 || !strings.Contains(got.errs[0].Error(), "dup") {
		t.Fatalf("errs = %v, want one naming %q", got.errs, "dup")
	}
	gotActual := indexActual([]string{"twice", "twice"})
	if len(gotActual.errs) != 1 || !strings.Contains(gotActual.errs[0].Error(), "twice") {
		t.Fatalf("errs = %v, want one naming %q", gotActual.errs, "twice")
	}
}

func keysOf(m map[string]*domain.Experiment) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
