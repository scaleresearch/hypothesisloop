package db

import (
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

func TestExperimentFilterClausesSearchIsParameterized(t *testing.T) {
	clauses, args := experimentFilterClauses(domain.ExperimentFilter{
		PlatformExperimentID: "pe-1",
		Search:               "'; DROP TABLE experiments; --",
	})
	joined := strings.Join(clauses, " AND ")
	if strings.Contains(joined, "DROP TABLE") {
		t.Fatalf("search text leaked into SQL clause instead of being bound as an arg: %q", joined)
	}
	if !strings.Contains(joined, "hypothesis ILIKE") || !strings.Contains(joined, "objective ILIKE") || !strings.Contains(joined, "theory ILIKE") {
		t.Fatalf("expected hypothesis/objective/theory ILIKE clause, got: %q", joined)
	}
	found := false
	for _, a := range args {
		if s, ok := a.(string); ok && strings.Contains(s, "DROP TABLE") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected search text to appear as a bound arg, got args: %v", args)
	}
}

func TestExperimentFilterClausesSearchOmittedWhenEmpty(t *testing.T) {
	clauses, _ := experimentFilterClauses(domain.ExperimentFilter{PlatformExperimentID: "pe-1"})
	joined := strings.Join(clauses, " AND ")
	if strings.Contains(joined, "ILIKE") {
		t.Fatalf("expected no ILIKE clause when Search is empty, got: %q", joined)
	}
}

func TestExperimentOrderByUnknownSortFallsBackToDefault(t *testing.T) {
	got := experimentOrderBy("not_a_real_field")
	want := "priority_score DESC, created_at ASC"
	if got != want {
		t.Fatalf("experimentOrderBy(unknown) = %q, want %q", got, want)
	}
}

func TestExperimentOrderByDescending(t *testing.T) {
	got := experimentOrderBy("-created_at")
	if got != "created_at DESC" {
		t.Fatalf("experimentOrderBy(-created_at) = %q, want %q", got, "created_at DESC")
	}
}
