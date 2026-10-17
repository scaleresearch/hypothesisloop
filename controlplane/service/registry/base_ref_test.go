package registry

import (
	"context"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// lineage is the graph and the jobs reduced to what a base commit is read from.
type lineage struct {
	Graph
	Store
	parents   map[string]string
	completed map[string]string
	baseline  string
}

func (l lineage) FirstParent(_ context.Context, id string) (string, error) { return l.parents[id], nil }
func (l lineage) LastCompletedCodeRef(_ context.Context, id string) (string, error) {
	return l.completed[id], nil
}
func (l lineage) BaselineCodeRef(context.Context, string) (string, error) { return l.baseline, nil }

// A new implementation starts from what its hypothesis builds on: the commit the parent's newest
// completed job ran, the parent's own base while it has none, and the baseline under the objective.
func TestBaseRefIsTheNearestCompletedCommitUpTheFirstParentsThenTheBaseline(t *testing.T) {
	l := lineage{
		parents:   map[string]string{"approach": "pe-1", "refined": "approach", "untested": "approach", "deeper": "untested", "fresh": "idle", "idle": "pe-1"},
		completed: map[string]string{"approach": "repo@approach"},
		baseline:  "repo@baseline",
	}
	svc := &Service{store: l, graph: l}
	for id, want := range map[string]string{
		"approach": "repo@baseline",
		"refined":  "repo@approach",
		"deeper":   "repo@approach",
		"fresh":    "repo@baseline",
	} {
		got, err := svc.BaseRef(context.Background(), &domain.Hypothesis{ID: id, PlatformExperimentID: "pe-1"})
		if err != nil || got != want {
			t.Errorf("BaseRef(%s) = %q, %v, want %q", id, got, err, want)
		}
	}

	l.baseline = ""
	svc = &Service{store: l, graph: l}
	if got, err := svc.BaseRef(context.Background(), &domain.Hypothesis{ID: "fresh", PlatformExperimentID: "pe-1"}); err != nil || got != "" {
		t.Errorf("BaseRef with no baseline declared = %q, %v, want none", got, err)
	}
	if _, err := svc.BaseRef(context.Background(), &domain.Hypothesis{ID: "orphan", PlatformExperimentID: "pe-1"}); err == nil {
		t.Error("BaseRef of a hypothesis that builds on nothing: got nil, want an error")
	}
}
