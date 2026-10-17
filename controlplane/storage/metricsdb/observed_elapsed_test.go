package metricsdb

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// ObserveSpan reads two series in one statement, and GreptimeDB fails the whole statement when
// either table is missing. Reading that as "never observed" billed a job with heartbeats as zero
// hours for as long as no job had posted a metric. Both tables exist from startup (see
// EnsureSchema), so a missing one is a fault.
func TestObserveSpanFailsOnAMissingTable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":3000,"error":"Failed to plan SQL: Table not found: greptime.public.experiment_metric_value","execution_time_ms":1}`))
	}))
	defer server.Close()

	now := time.Now().UTC()
	if _, err := New(server.URL).ObserveSpan(context.Background(), "exp-1", now.Add(-time.Hour), now, 3*time.Minute); err == nil {
		t.Fatal("ObserveSpan: got = nil, want an error for a missing observation table")
	}
}
