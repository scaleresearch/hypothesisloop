package ranktail

import (
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"
)

func ranks(sections []Section) []int {
	out := make([]int, 0, len(sections))
	for _, s := range sections {
		out = append(out, s.Rank)
	}
	return out
}

func repeat(where string) func(n int) ([]string, error) {
	return func(n int) ([]string, error) { return slices.Repeat([]string{where}, n), nil }
}

// Every reader of a tail reads its end, so the rank that explains the job comes last: the first
// rank to fail, the ranks that died after it before it, rank 0 before them and the rest first.
func TestPickPutsTheFirstFailureLastAndRankZeroBeforeTheFailures(t *testing.T) {
	t0 := time.Now()
	sections := []Section{
		{Rank: 3, Exit: 1, FailedAt: t0.Add(time.Second)},
		{Rank: 1},
		{Rank: 2, Exit: 137, FailedAt: t0},
		{Rank: 0},
	}
	picked, omitted := Pick(sections, 10)
	if got, want := ranks(picked), []int{1, 0, 3, 2}; !reflect.DeepEqual(got, want) || omitted != 0 {
		t.Fatalf("ranks = %v with %d omitted, want %v with none", got, omitted, want)
	}
}

// Failures at the same moment are ordered by rank, the lowest last.
func TestPickOrdersSimultaneousFailuresByRank(t *testing.T) {
	t0 := time.Now()
	picked, _ := Pick([]Section{{Rank: 1, Exit: 1, FailedAt: t0}, {Rank: 2, Exit: 1, FailedAt: t0}}, 10)
	if got, want := ranks(picked), []int{2, 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ranks = %v, want %v", got, want)
	}
}

// A gang larger than the budget shows the failed ranks and rank 0 before any other rank.
func TestPickKeepsFailuresAndRankZeroWhenNotEveryRankFits(t *testing.T) {
	var sections []Section
	for i := 11; i >= 0; i-- {
		sections = append(sections, Section{Rank: i})
	}
	sections[0].Exit = 1
	picked, omitted := Pick(sections, 3)
	if got, want := ranks(picked), []int{1, 0, 11}; !reflect.DeepEqual(got, want) || omitted != 9 {
		t.Fatalf("ranks = %v with %d omitted, want %v with 9", got, omitted, want)
	}
}

func TestHeaderNamesTheRankItsGroupAndWhereItRan(t *testing.T) {
	for _, tc := range []struct {
		s    Section
		want string
	}{
		{Section{Rank: 1, Where: "pod exp-gang-1"}, "--- rank 1 (pod exp-gang-1) ---"},
		{Section{Rank: 0, Group: "learner", Where: "pod learner-0"}, "--- rank 0 (group learner, pod learner-0) ---"},
		{Section{Rank: 1, Exit: 3, Where: "instance hl-job-x"}, "--- rank 1 failed, exit 3 (instance hl-job-x) ---"},
		{Section{Rank: 2, Group: "actor", Exit: 137, Where: "pod actor-1"}, "--- rank 2 failed, exit 137 (group actor, pod actor-1) ---"},
	} {
		if got := tc.s.Header(); got != tc.want {
			t.Errorf("header = %q, want %q", got, tc.want)
		}
	}
}

// The sections share one budget, and what a short section leaves goes to the ones after it.
func TestAssembleSharesTheBudgetAndPassesUnusedLinesOn(t *testing.T) {
	asked := map[string]int{}
	read := func(where string) func(n int) ([]string, error) {
		return func(n int) ([]string, error) {
			asked[where] = n
			if where == "a" {
				return []string{"a only"}, nil
			}
			return repeat(where)(n)
		}
	}
	sections := []Section{{Rank: 1, Where: "a", Read: read("a")}, {Rank: 0, Where: "b", Read: read("b")}, {Rank: 2, Exit: 1, Where: "c", Read: read("c")}}
	lines, err := assemble(sections, 4, 31)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]int{"a": 9, "b": 13, "c": 13}; !reflect.DeepEqual(asked, want) {
		t.Errorf("asked %v, want %v", asked, want)
	}
	if len(lines) != 31 {
		t.Errorf("tail has %d lines, want the budget of 31", len(lines))
	}
	if lines[0] != "--- 4 more ranks not shown ---" || lines[1] != sections[0].Header() || lines[len(lines)-14] != sections[2].Header() {
		t.Errorf("tail = %q, want the omission line, then each section under its header", lines)
	}
}

// A rank that wrote nothing yet has no section, header included.
func TestTailLeavesOutARankWithNoLines(t *testing.T) {
	sections := []Section{
		{Rank: 1, Where: "instance one", Read: func(int) ([]string, error) { return nil, nil }},
		{Rank: 0, Where: "instance zero", Read: repeat("zero")},
	}
	lines, err := Tail(sections, 20)
	if err != nil {
		t.Fatal(err)
	}
	if want := append([]string{"--- rank 0 (instance zero) ---"}, slices.Repeat([]string{"zero"}, 19)...); !reflect.DeepEqual(lines, want) {
		t.Errorf("tail = %q, want %q", lines, want)
	}
}

func TestTailFailsWhenARankCannotBeRead(t *testing.T) {
	broken := errors.New("unreachable")
	sections := []Section{{Rank: 0, Where: "instance zero", Read: func(int) ([]string, error) { return nil, broken }}}
	if _, err := Tail(sections, 20); !errors.Is(err, broken) {
		t.Fatalf("err = %v, want %v", err, broken)
	}
}
