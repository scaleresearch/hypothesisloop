package metricsdb

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The shared row reader requires greptime_timestamp and greptime_value columns and errors
// without them. A query that names its columns anything else therefore fails at runtime, not at
// compile time — and this one feeds job-to-node attribution, so its failure aborts the
// disbalance pass entirely rather than surfacing as a wrong answer.
func TestLatestExperimentNodeAsksForColumnsTheReaderCanParse(t *testing.T) {
	var asked string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.Query().Get("sql")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output":[{"records":{"schema":{"column_schemas":[
			{"name":"node","data_type":"String"},
			{"name":"greptime_timestamp","data_type":"TimestampMillisecond"},
			{"name":"greptime_value","data_type":"Int64"}]},
			"rows":[["node-b",1700000000000,1]]}}]}`))
	}))
	defer server.Close()

	node, found, err := LatestExperimentNode(context.Background(), server.URL, "exp-1", time.Now().UTC().Add(-time.Hour), time.Now().UTC())
	if err != nil {
		t.Fatalf("LatestExperimentNode: %v", err)
	}
	if !found || node != "node-b" {
		t.Fatalf("node = %q found = %v, want node-b", node, found)
	}
	for _, column := range []string{"greptime_timestamp", "greptime_value"} {
		if !strings.Contains(asked, column) {
			t.Errorf("query does not select %s, which the row reader requires: %s", column, asked)
		}
	}
}

// Ordering must come from each node's newest real sample. Asking on a grid whose step equalled
// the lookback put every node's last point on the same timestamp, so the winner was whichever
// series came back first — after a reschedule, as likely the node the job left as the one it
// moved to.
func TestLatestExperimentNodeOrdersByNewestSample(t *testing.T) {
	var asked string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.Query().Get("sql")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output":[{"records":{"schema":{"column_schemas":[
			{"name":"node","data_type":"String"},
			{"name":"greptime_timestamp","data_type":"TimestampMillisecond"},
			{"name":"greptime_value","data_type":"Int64"}]},
			"rows":[["node-new",1700000000000,1]]}}]}`))
	}))
	defer server.Close()

	if _, _, err := LatestExperimentNode(context.Background(), server.URL, "exp-1", time.Now().UTC().Add(-time.Hour), time.Now().UTC()); err != nil {
		t.Fatalf("LatestExperimentNode: %v", err)
	}
	if !strings.Contains(asked, "MAX(greptime_timestamp)") || !strings.Contains(asked, "ORDER BY 2 DESC") {
		t.Errorf("query does not order nodes by their newest sample: %s", asked)
	}
}
