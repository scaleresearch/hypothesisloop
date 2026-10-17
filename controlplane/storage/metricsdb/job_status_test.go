package metricsdb

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

func TestRecordJobStatusesRejectsInvalidPhaseWithoutWriting(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	now := time.Now()
	err := New(server.URL).RecordJobStatuses(context.Background(), "cluster-a", now, []JobStatusSample{{
		ExperimentID: "exp-1", ClusterName: "cluster-a", Phase: "invented", At: now,
	}})
	if err == nil {
		t.Fatal("RecordJobStatuses accepted an unknown phase")
	}
	if requests.Load() != 0 {
		t.Fatalf("invalid snapshot made %d metrics writes", requests.Load())
	}
}

func TestRecordJobStatusesWritesEmptyCompleteSnapshot(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	if err := New(server.URL).RecordJobStatuses(context.Background(), "cluster-a", time.Now(), nil); err != nil {
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

	phase, found, err := New(server.URL).LatestJobPhase(context.Background(), "exp-1", "cluster-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !found || phase != domain.JobPhaseGone {
		t.Fatalf("LatestJobPhase = (%v, %v), want (gone, true)", phase, found)
	}
}

func TestLatestJobPhaseKeepsJobReportedInCurrentSnapshot(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"output":[{"records":{"rows":[[20,2,20]]}}]}`)
	}))
	defer server.Close()

	phase, found, err := New(server.URL).LatestJobPhase(context.Background(), "exp-1", "cluster-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !found || phase != domain.JobPhaseRunning {
		t.Fatalf("LatestJobPhase = (%v, %v), want (running, true)", phase, found)
	}
}
