package db

import (
	"reflect"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
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
	if !strings.Contains(joined, "hypothesis ILIKE") || !strings.Contains(joined, "theory ILIKE") {
		t.Fatalf("expected hypothesis/theory ILIKE clause, got: %q", joined)
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

func TestExperimentOrderByUnknownSortFallsBackToDefault(t *testing.T) {
	got := experimentOrderBy("not_a_real_field")
	want := "priority_score DESC, created_at ASC"
	if got != want {
		t.Fatalf("experimentOrderBy(unknown) = %q, want %q", got, want)
	}
}

func TestACancelledJobIsCountedAndFilteredAsCancelled(t *testing.T) {
	stats := newExperimentStats()
	for _, row := range [][2]string{{"EVICTED", "cancelled"}, {"REJECTED", "cancelled"}, {"EVICTED", "silent"}, {"REJECTED", ""}} {
		tallyExperimentStats(stats, row[0], "burst", row[1], domain.TypeKindAccelerator, 1, 1)
	}
	if want := map[string]int{"CANCELLED": 2, "EVICTED": 1, "REJECTED": 1}; !reflect.DeepEqual(stats.ByStatus, want) {
		t.Errorf("by status: got = %v, want = %v", stats.ByStatus, want)
	}
	if want := map[string]int{"silent": 1}; !reflect.DeepEqual(stats.EvictionsByReason, want) {
		t.Errorf("evictions by reason: got = %v, want = %v", stats.EvictionsByReason, want)
	}

	clauses, args := experimentFilterClauses(domain.ExperimentFilter{Scope: domain.AllTenants, Status: domain.StatusCancelled})
	if joined := strings.Join(clauses, " AND "); strings.Contains(joined, "status =") || !reflect.DeepEqual(args, []any{"cancelled"}) {
		t.Errorf("status CANCELLED: got = %q %v, want the cancelled reason alone", joined, args)
	}
	clauses, args = experimentFilterClauses(domain.ExperimentFilter{Scope: domain.AllTenants, Status: domain.StatusEvicted, AgentID: "a", PlatformExperimentID: "pe-1"})
	if joined := strings.Join(clauses, " AND "); !strings.Contains(joined, "status = $2 AND "+evictionReasonCode+" <> $3") ||
		!reflect.DeepEqual(args, []any{"a", "EVICTED", "cancelled", "pe-1"}) || !strings.Contains(joined, "platform_experiment_id = $4") {
		t.Errorf("status EVICTED: got = %q %v, want the stored status without the cancelled jobs", joined, args)
	}
}
