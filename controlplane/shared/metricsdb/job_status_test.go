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

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/workload"
)

func TestLatestJobPhaseReadsOneConsistentSnapshot(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query().Get("sql")
		for _, metric := range []string{clusterJobPhaseMetric, clusterJobObservedAtMetric, clusterJobSnapshotMetric} {
			if !strings.Contains(query, metric) {
				t.Errorf("status query is missing %s: %s", metric, query)
			}
		}
		requests.Add(1)
		_, _ = w.Write([]byte(`{"output":[{"records":{"rows":[]}}]}`))
	}))
	defer server.Close()

	if _, _, err := LatestJobPhase(context.Background(), server.URL, "exp-1", "cluster-a", time.Minute); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatalf("LatestJobPhase made %d queries, want one consistent snapshot query", requests.Load())
	}
}

func TestRecordJobStatusesRejectsInvalidPhaseWithoutWriting(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	now := time.Now()
	err := RecordJobStatuses(context.Background(), server.URL, "cluster-a", now, []JobStatusSample{{
		ExperimentID: "exp-1", ClusterName: "cluster-a", Phase: "invented", At: now,
	}})
	if err == nil {
		t.Fatal("RecordJobStatuses accepted an unknown phase")
	}
	if requests.Load() != 0 {
		t.Fatalf("invalid snapshot made %d metrics writes", requests.Load())
	}
}

func TestRecordJobStatusesWritesPhaseNodeAndTypeInOneRequest(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	now := time.Now()
	err := RecordJobStatuses(context.Background(), server.URL, "cluster-a", now, []JobStatusSample{{
		ExperimentID: "exp-1", ClusterName: "cluster-a", Phase: "running",
		AdmittedNode: "node-a", AdmittedAcceleratorType: "H100", At: now,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatalf("snapshot made %d writes, want exactly one", requests.Load())
	}
}

func TestRecordJobStatusesWritesEmptyCompleteSnapshot(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	if err := RecordJobStatuses(context.Background(), server.URL, "cluster-a", time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatalf("empty complete snapshot made %d writes, want exactly one", requests.Load())
	}
}

func TestLatestJobPhaseUsesNewerCompleteSnapshotToProveGone(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"output":[{"records":{"rows":[[20,null,null]]}}]}`)
	}))
	defer server.Close()

	phase, found, err := LatestJobPhase(context.Background(), server.URL, "exp-1", "cluster-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !found || phase != workload.JobPhaseGone {
		t.Fatalf("LatestJobPhase = (%v, %v), want (gone, true)", phase, found)
	}
}

func TestLatestJobPhaseKeepsJobReportedInCurrentSnapshot(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"output":[{"records":{"rows":[[20,2,20]]}}]}`)
	}))
	defer server.Close()

	phase, found, err := LatestJobPhase(context.Background(), server.URL, "exp-1", "cluster-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !found || phase != workload.JobPhaseRunning {
		t.Fatalf("LatestJobPhase = (%v, %v), want (running, true)", phase, found)
	}
}
