package queuebackend

import (
	"context"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

type constructorStore struct{}

func (constructorStore) SumDesiredFootprintByCluster(context.Context) (map[string]domain.Footprint, error) {
	return map[string]domain.Footprint{}, nil
}

func TestNewRejectsMissingAuthoritativeInputs(t *testing.T) {
	tests := []struct {
		name       string
		store      Store
		metricsURL string
		window     time.Duration
	}{
		{name: "store", metricsURL: "http://metrics", window: time.Second},
		{name: "metrics URL", store: constructorStore{}, window: time.Second},
		{name: "liveness window", store: constructorStore{}, metricsURL: "http://metrics"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New(tt.store, tt.metricsURL, tt.window); err == nil {
				t.Fatalf("New accepted missing %s", tt.name)
			}
		})
	}
}

func TestMinimumFootprintRequiresBothDesiredAndActualCapacity(t *testing.T) {
	cpu := domain.ResourceKey{Kind: domain.ResourceKindCPU}
	h100 := domain.ResourceKey{Kind: domain.ResourceKindAccelerator, Flavor: "H100"}
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
	b := &Backend{}
	inconsistent := b.validateCapacitySnapshot(
		map[string]bool{"cluster-a": true},
		map[string]float64{"cluster-a": 4}, map[string]int64{"cluster-a": 8000},
		map[string]map[string]int64{"cluster-a": {"flavor-h100": 1}},
		map[string]map[string]int64{"cluster-a": {"flavor-h100": 2}},
		map[string]int64{"cluster-a": 8}, map[string]int64{"cluster-a": 16},
		map[string]int64{}, map[string]int64{"cluster-a": 32},
	)
	if inconsistent["cluster-a"] == nil {
		t.Fatal("validateCapacitySnapshot accepted missing storage availability")
	}
}

func TestValidateCapacitySnapshotAllowsClusterSpecificAcceleratorTypes(t *testing.T) {
	b := &Backend{}
	inconsistent := b.validateCapacitySnapshot(
		map[string]bool{"cluster-a": true},
		map[string]float64{"cluster-a": 4}, map[string]int64{"cluster-a": 8000},
		map[string]map[string]int64{"cluster-a": {"flavor-h100": 1}},
		map[string]map[string]int64{"cluster-a": {"flavor-h100": 2}},
		map[string]int64{"cluster-a": 8}, map[string]int64{"cluster-a": 16},
		map[string]int64{"cluster-a": 16}, map[string]int64{"cluster-a": 32},
	)
	if len(inconsistent) != 0 {
		t.Fatalf("validateCapacitySnapshot rejected a valid heterogeneous cluster: %v", inconsistent)
	}
}

func TestAnInconsistentClusterSnapshotCondemnsOnlyThatCluster(t *testing.T) {
	b := &Backend{}
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
