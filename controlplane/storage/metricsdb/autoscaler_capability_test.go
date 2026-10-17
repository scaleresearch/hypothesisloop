package metricsdb

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A cluster that never reported cluster_autoscaler_enabled must read as "not autoscaled" — the
// fail-closed rule autoscaler.md requires so a false positive never costs a job a wasted
// scale-up deadline. Two clusters here: one reporting true, one absent entirely.
func TestLiveClusterAutoscalerCapabilityIsFailClosedForAnUnreportedCluster(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"cluster_name":"cluster-a"},"value":[0,"1"]}]}}`)
	}))
	defer server.Close()

	got, err := LiveClusterAutoscalerCapability(context.Background(), server.URL, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !got["cluster-a"] {
		t.Errorf("cluster-a reported true, want true in the map")
	}
	if got["cluster-b"] {
		t.Error("cluster-b never reported — must be absent/false, never true")
	}
	if _, present := got["cluster-b"]; present {
		t.Error("an unreported cluster must be absent from the map, not present with false")
	}
}

// LiveClusterIDs must join the heartbeat's cluster_id label onto the display name, and must
// tolerate an older-runtime heartbeat that carries no cluster_id at all (empty label) by simply
// omitting that cluster rather than erroring the whole read.
func TestLiveClusterIDsOmitsClustersWithNoReportedIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"output":[{"records":{"schema":{"column_schemas":[{"name":"cluster_name"},{"name":"cluster_id"},{"name":"greptime_timestamp"},{"name":"greptime_value"}]},"rows":[["cluster-a","uid-123",0,1],["cluster-b","",0,1]],"total_rows":2}}]}`)
	}))
	defer server.Close()

	got, err := LiveClusterIDs(context.Background(), server.URL, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if got["cluster-a"] != "uid-123" {
		t.Errorf("cluster-a id = %q, want uid-123", got["cluster-a"])
	}
	if _, present := got["cluster-b"]; present {
		t.Error("cluster-b reported no cluster_id — must be absent, not a zero-value entry")
	}
}
