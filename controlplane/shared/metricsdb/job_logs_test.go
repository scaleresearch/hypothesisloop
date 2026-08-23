package metricsdb

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Metric tables are created by the first write to them, so on a deployment where no job has
// reported yet, every read of one comes back "table not found". Treating that as a failure meant
// settlement could not compute observed hours and no job's quota was ever refunded until some
// unrelated job happened to post the first metric — a cold-start bug that fixes itself just late
// enough to be hard to attribute. "Nothing has been written" and "this job wrote nothing" are the
// same answer, and it is zero rows.
func TestQuerySQLRowsTreatsAMissingTableAsNoRows(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":4001,"error":"Failed to plan SQL: Table not found: greptime.public.experiment_metric_value"}`))
	}))
	defer server.Close()

	rows, err := querySQLRows(context.Background(), server.URL, "SELECT 1")
	if err != nil {
		t.Fatalf("querySQLRows returned %v, want no error — an absent table is an empty one", err)
	}
	if len(rows) != 0 {
		t.Errorf("got %d row(s), want 0", len(rows))
	}
}

// Every other failure still is one. Swallowing them would turn an unreachable or broken metrics
// store into "this job consumed nothing", which silently refunds work that really was done.
func TestQuerySQLRowsStillFailsOnAnyOtherError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":3000,"error":"Internal error: storage is unavailable"}`))
	}))
	defer server.Close()

	if _, err := querySQLRows(context.Background(), server.URL, "SELECT 1"); err == nil {
		t.Fatal("querySQLRows swallowed a storage failure — that bills a real run as zero hours")
	}
}
