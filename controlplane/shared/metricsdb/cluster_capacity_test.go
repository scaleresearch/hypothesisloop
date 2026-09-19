package metricsdb

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gogo/protobuf/proto"
	"github.com/golang/snappy"
)

// snapshotServer answers the heartbeat read with cluster-a's latest heartbeat at t=20 and every
// other query with sampleRows, failing the test when a capacity read is not pinned to it.
func snapshotServer(t *testing.T, sampleRows string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query().Get("sql")
		if strings.Contains(query, "FROM "+clusterHeartbeatMetric) {
			if strings.Contains(query, clusterNodeAcceleratorAvailableMetric) {
				t.Errorf("heartbeat and capacity are read in one statement, which is not atomic across tables: %s", query)
			}
			_, _ = fmt.Fprint(w, `{"output":[{"records":{"schema":{"column_schemas":[{"name":"cluster_name"},{"name":"greptime_timestamp"},{"name":"greptime_value"}]},"rows":[["cluster-a",20,1.0]],"total_rows":1}}]}`)
			return
		}
		if !strings.Contains(query, "FROM "+clusterNodeAcceleratorAvailableMetric) || !strings.Contains(query, "(cluster_name = 'cluster-a' AND greptime_timestamp = 20)") {
			t.Errorf("capacity is not read at the heartbeat already seen: %s", query)
		}
		_, _ = fmt.Fprintf(w, `{"output":[{"records":{"schema":{"column_schemas":[{"name":"cluster_name"},{"name":"node"},{"name":"accelerator_type"},{"name":"greptime_timestamp"},{"name":"greptime_value"}]},"rows":[%s]}}]}`, sampleRows)
	}))
}

func TestLiveClusterNodeCapacityDropsNodeAbsentFromLatestSnapshot(t *testing.T) {
	server := snapshotServer(t, "")
	defer server.Close()

	got, err := LiveClusterNodeAcceleratorCapacity(context.Background(), server.URL, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("stale removed-node capacity survived complete snapshot: %v", got)
	}
}

func TestLiveClusterNodeCapacityReadsSamplesAtLatestHeartbeat(t *testing.T) {
	server := snapshotServer(t, `["cluster-a","node-a","test-type",20,2.0]`)
	defer server.Close()

	got, err := LiveClusterNodeAcceleratorCapacity(context.Background(), server.URL, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if got["cluster-a"]["node-a"]["test-type"] != 2 {
		t.Fatalf("current node capacity = %v, want 2", got)
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

func TestLiveClusterAcceleratorCapacityBeforeAnyReportIsEmpty(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"code":4001,"error":"Failed to plan SQL: Table not found: greptime.public.cluster_agent_heartbeat"}`)
	}))
	defer server.Close()
	available, total, err := LiveClusterAcceleratorAvailableAndTotal(context.Background(), server.URL, time.Minute)
	if err != nil || len(available) != 0 || len(total) != 0 {
		t.Fatalf("got (%v, %v, %v), want empty capacity and no error", available, total, err)
	}
}
