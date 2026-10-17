package scheduler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/metricsdb"
)

type constructorStore struct{}

func (constructorStore) SumDesiredFootprintByCluster(context.Context) (map[string]domain.Footprint, error) {
	return map[string]domain.Footprint{}, nil
}

func TestNewClusterReportsRejectsMissingAuthoritativeInputs(t *testing.T) {
	tests := []struct {
		name    string
		store   DesiredFootprints
		metrics *metricsdb.Store
		window  time.Duration
	}{
		{name: "store", metrics: metricsdb.New("http://metrics"), window: time.Second},
		{name: "metrics store", store: constructorStore{}, window: time.Second},
		{name: "liveness window", store: constructorStore{}, metrics: metricsdb.New("http://metrics")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewClusterReports(tt.store, tt.metrics, tt.window); err == nil {
				t.Fatalf("NewClusterReports accepted missing %s", tt.name)
			}
		})
	}
}

func TestMinimumFootprintRequiresBothDesiredAndActualCapacity(t *testing.T) {
	cpu := domain.ResourceKey{Kind: domain.ResourceKindCPU}
	h100 := domain.ResourceKey{Kind: domain.ResourceKindAccelerator, Name: "H100"}
	desiredFree := domain.Footprint{
		cpu:  8000,
		h100: 4,
	}
	actualFree := domain.Footprint{
		cpu:  2000,
		h100: 3,
	}

	got := minimumFootprint(desiredFree, actualFree)
	if got[cpu] != 2000 || got[h100] != 3 {
		t.Fatalf("minimumFootprint = %v, want cpu=2000 accelerator=3", got)
	}
}

func TestMinimumFootprintDesiredClaimWinsBeforeActualCatchesUp(t *testing.T) {
	cpu := domain.ResourceKey{Kind: domain.ResourceKindCPU}
	desiredFree := domain.Footprint{cpu: 1000}
	actualFree := domain.Footprint{cpu: 8000}
	if got := minimumFootprint(desiredFree, actualFree)[cpu]; got != 1000 {
		t.Fatalf("minimumFootprint cpu = %d, want desired-state ceiling 1000", got)
	}
}

func TestValidateCapacitySnapshotRejectsMissingAuthoritativeMetric(t *testing.T) {
	b := &ClusterReports{}
	inconsistent := b.validateCapacitySnapshot(
		map[string]bool{"cluster-a": true},
		map[string]float64{"cluster-a": 4}, map[string]int64{"cluster-a": 8000},
		map[string]map[string]int64{"cluster-a": {"type-h100": 1}},
		map[string]map[string]int64{"cluster-a": {"type-h100": 2}},
		map[string]int64{"cluster-a": 8}, map[string]int64{"cluster-a": 16},
		map[string]int64{}, map[string]int64{"cluster-a": 32},
	)
	if inconsistent["cluster-a"] == nil {
		t.Fatal("validateCapacitySnapshot accepted missing storage availability")
	}
}

func TestValidateCapacitySnapshotAllowsClusterSpecificAcceleratorTypes(t *testing.T) {
	b := &ClusterReports{}
	inconsistent := b.validateCapacitySnapshot(
		map[string]bool{"cluster-a": true},
		map[string]float64{"cluster-a": 4}, map[string]int64{"cluster-a": 8000},
		map[string]map[string]int64{"cluster-a": {"type-h100": 1}},
		map[string]map[string]int64{"cluster-a": {"type-h100": 2}},
		map[string]int64{"cluster-a": 8}, map[string]int64{"cluster-a": 16},
		map[string]int64{"cluster-a": 16}, map[string]int64{"cluster-a": 32},
	)
	if len(inconsistent) != 0 {
		t.Fatalf("validateCapacitySnapshot rejected a valid heterogeneous cluster: %v", inconsistent)
	}
}

func TestAnInconsistentClusterSnapshotCondemnsOnlyThatCluster(t *testing.T) {
	b := &ClusterReports{}
	inconsistent := b.validateCapacitySnapshot(
		map[string]bool{"cluster-a": true, "cluster-b": true},
		map[string]float64{"cluster-a": 4, "cluster-b": 64}, map[string]int64{"cluster-a": 8000, "cluster-b": 8000},
		nil, nil,
		map[string]int64{"cluster-a": 8, "cluster-b": 8}, map[string]int64{"cluster-a": 16, "cluster-b": 16},
		map[string]int64{"cluster-a": 16, "cluster-b": 16}, map[string]int64{"cluster-a": 32, "cluster-b": 32},
	)
	if inconsistent["cluster-b"] == nil {
		t.Error("cluster-b reports more free CPU than it has installed and was accepted")
	}
	if err := inconsistent["cluster-a"]; err != nil {
		t.Errorf("cluster-a was rejected for cluster-b's snapshot: %v", err)
	}
}

// A job preempted and claimed again between two status snapshots was read as ended by the
// snapshot that held the end of the admission before: the watcher marked the new one RUNNING and
// then FAILED. A phase is read only from a snapshot taken since the job was claimed.
func TestAJobsPhaseIsReadOnlyFromSnapshotsSinceItWasClaimed(t *testing.T) {
	var queries []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.Query().Get("sql"))
		_, _ = w.Write([]byte(`{"output":[{"records":{"rows":[[20,3,20]]}}]}`))
	}))
	defer server.Close()
	b := &ClusterReports{metrics: metricsdb.New(server.URL), connectedWithin: time.Minute}
	claimed := func(ago time.Duration) *domain.Experiment {
		at := time.Now().Add(-ago)
		return &domain.Experiment{ID: "exp-1", ClusterName: "cluster-a", SubmittedAt: &at}
	}

	if _, found, err := b.LatestJobPhase(context.Background(), claimed(500*time.Millisecond)); err != nil || found || len(queries) != 0 {
		t.Fatalf("claimed half a second ago: found %v, err %v, %d queries, want no phase and no read of an older snapshot", found, err, len(queries))
	}
	if _, found, err := b.LatestJobPhase(context.Background(), claimed(5500*time.Millisecond)); err != nil || !found || !strings.Contains(queries[0], "INTERVAL '5 seconds'") {
		t.Fatalf("claimed 5.5s ago: found %v, err %v, query %q, want the snapshots of the last 5 seconds", found, err, queries)
	}
	if _, found, err := b.LatestJobPhase(context.Background(), claimed(time.Hour)); err != nil || !found || !strings.Contains(queries[1], "INTERVAL '60 seconds'") {
		t.Fatalf("claimed an hour ago: found %v, err %v, query %q, want the liveness window", found, err, queries)
	}
	if _, _, err := b.LatestJobPhase(context.Background(), &domain.Experiment{ID: "exp-1", ClusterName: "cluster-a"}); err == nil {
		t.Fatal("a job with no submission time was given a phase")
	}
}

func TestInconsistentSnapshotsNamesEachSkippedClusterWithItsReason(t *testing.T) {
	b := &ClusterReports{}
	var err error = InconsistentSnapshots(b.validateCapacitySnapshot(
		map[string]bool{"cluster-b": true},
		map[string]float64{"cluster-b": 64}, map[string]int64{"cluster-b": 8000},
		nil, nil,
		map[string]int64{"cluster-b": 8}, map[string]int64{"cluster-b": 16},
		map[string]int64{"cluster-b": 16}, map[string]int64{"cluster-b": 32},
	))
	var skipped InconsistentSnapshots
	if !errors.As(err, &skipped) || skipped["cluster-b"] == nil {
		t.Fatalf("err = %v, want it to carry cluster-b's inconsistency", err)
	}
	if !strings.Contains(err.Error(), "exceeds total") {
		t.Fatalf("err = %q, want the specific inconsistency", err)
	}
}
