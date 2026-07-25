package metricsdb

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestLiveClusterNodeCapacityDropsNodeAbsentFromLatestSnapshot(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		query := r.URL.Query().Get("sql")
		if !strings.Contains(query, "FROM "+clusterHeartbeatMetric) || !strings.Contains(query, "FROM "+clusterNodeAcceleratorAvailableMetric) || !strings.Contains(query, "metric.greptime_timestamp = heartbeat.snapshot_at") {
			t.Errorf("query does not correlate capacity with its cluster snapshot: %s", query)
		}
		_, _ = fmt.Fprint(w, `{"output":[{"records":{"schema":{"column_schemas":[{"name":"accelerator_type"},{"name":"cluster_name"},{"name":"greptime_timestamp"},{"name":"greptime_value"}]},"rows":[],"total_rows":0}}]}`)
	}))
	defer server.Close()

	got, err := LiveClusterNodeAcceleratorCapacity(context.Background(), server.URL, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("stale removed-node capacity survived complete snapshot: %v", got)
	}
	if requests.Load() != 1 {
		t.Fatalf("requests = %d, want one authoritative query", requests.Load())
	}
}

func TestLiveClusterNodeCapacityKeepsNodeInLatestSnapshot(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = fmt.Fprint(w, `{"output":[{"records":{"schema":{"column_schemas":[{"name":"cluster_name"},{"name":"node"},{"name":"accelerator_type"},{"name":"greptime_timestamp"},{"name":"greptime_value"}]},"rows":[["cluster-a","node-a","test-type",20,2.0]],"total_rows":1}}]}`)
	}))
	defer server.Close()

	got, err := LiveClusterNodeAcceleratorCapacity(context.Background(), server.URL, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if got["cluster-a"]["node-a"]["test-type"] != 2 {
		t.Fatalf("current node capacity = %v, want 2", got)
	}
	if requests.Load() != 1 {
		t.Fatalf("requests = %d, want one authoritative query", requests.Load())
	}
}
