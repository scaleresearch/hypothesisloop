package metricsdb

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gogo/protobuf/proto"
	"github.com/golang/snappy"
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

func TestRecordClusterCapacitySnapshotWritesHeartbeatLast(t *testing.T) {
	var writes [][]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := snappy.Decode(nil, raw)
		if err != nil {
			t.Fatal(err)
		}
		var req rwWriteRequest
		if err := proto.Unmarshal(decoded, &req); err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, ts := range req.Timeseries {
			for _, l := range ts.Labels {
				if l.Name == "__name__" {
					names = append(names, l.Value)
				}
			}
		}
		writes = append(writes, names)
	}))
	defer server.Close()

	err := RecordClusterCapacitySnapshot(context.Background(), server.URL, ClusterCapacitySnapshot{
		ClusterName: "c", At: time.Now(),
		AcceleratorAvailable: map[string]int64{"gpu": 1}, AcceleratorTotal: map[string]int64{"gpu": 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(writes) != 2 || len(writes[1]) != 1 || writes[1][0] != clusterHeartbeatMetric {
		t.Fatalf("writes = %v, want the snapshot and then the heartbeat alone", writes)
	}
	for _, name := range writes[0] {
		if name == clusterHeartbeatMetric {
			t.Fatalf("heartbeat written with the samples it vouches for: %v", writes[0])
		}
	}
}
