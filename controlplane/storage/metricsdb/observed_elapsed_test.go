package metricsdb

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ObserveSpan reads two series in one statement, and GreptimeDB fails the whole statement when
// either table is missing. Reading that as "never observed" billed a job with heartbeats as zero
// hours for as long as no job had posted a metric. Both tables exist from startup (see
// EnsureObservationTables), so a missing one is a fault.
func TestObserveSpanFailsOnAMissingTable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":3000,"error":"Failed to plan SQL: Table not found: greptime.public.experiment_metric_value","execution_time_ms":1}`))
	}))
	defer server.Close()

	now := time.Now().UTC()
	if _, err := ObserveSpan(context.Background(), server.URL, "exp-1", now.Add(-time.Hour), now, 3*time.Minute); err == nil {
		t.Fatal("ObserveSpan: got = nil, want an error for a missing observation table")
	}
}

func TestEnsureObservationTablesCreatesBothSeries(t *testing.T) {
	var statements []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		statements = append(statements, r.FormValue("sql"))
		_, _ = w.Write([]byte(`{"output":[{"affectedrows":0}]}`))
	}))
	defer server.Close()

	if err := EnsureObservationTables(context.Background(), server.URL); err != nil {
		t.Fatalf("EnsureObservationTables: got = %v, want = nil", err)
	}
	all := strings.Join(statements, "\n")
	for _, table := range []string{aliveHeartbeatMetric, admissionMetric, ExperimentMetricValue} {
		if !strings.Contains(all, "CREATE TABLE IF NOT EXISTS "+table+" ") {
			t.Errorf("no CREATE TABLE issued for %s", table)
		}
	}
}
