package scheduler

import (
	"go.uber.org/zap"

	"context"
	"sync"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

type concurrentAdmissionStore struct {
	Store
	mu          sync.Mutex
	experiments map[string]*domain.Experiment
	claimed     int64
}

func (s *concurrentAdmissionStore) GetExperiment(_ context.Context, id string) (*domain.Experiment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	exp := *s.experiments[id]
	return &exp, nil
}

func (s *concurrentAdmissionStore) ClaimSubmitted(ctx context.Context, id, clusterName string, capacityAvailable func(context.Context, []*domain.Experiment) (bool, error)) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.experiments[id].Status != domain.StatusQueued {
		return false, nil
	}
	desired := make([]*domain.Experiment, 0)
	for _, exp := range s.experiments {
		if exp.Status == domain.StatusSubmitted && exp.ClusterName == clusterName {
			desired = append(desired, exp)
		}
	}
	available, err := capacityAvailable(ctx, desired)
	if err != nil || !available {
		return false, err
	}
	s.experiments[id].Status = domain.StatusSubmitted
	s.experiments[id].ClusterName = clusterName
	s.claimed += int64(s.experiments[id].AcceleratorCount)
	return true, nil
}

type concurrentAdmissionWorkload struct {
	WorkloadClient
	store *concurrentAdmissionStore
}

func (w *concurrentAdmissionWorkload) GetFlavorCapacity(context.Context) (map[string]domain.Footprint, map[string]domain.Footprint, error) {
	key := domain.ResourceKey{Kind: domain.ResourceKindAccelerator, Flavor: "nvidia.com/gpu.product=nvidia-l40"}
	available := domain.Footprint{key: 8 - w.store.claimed}
	return map[string]domain.Footprint{"cluster-a": available}, nil, nil
}

func (w *concurrentAdmissionWorkload) CreateWorkload(context.Context, *domain.Experiment) error {
	return nil
}

func (w *concurrentAdmissionWorkload) GetAcceleratorCapacityByNode(context.Context) (map[string]map[string]map[string]int64, error) {
	return map[string]map[string]map[string]int64{
		"cluster-a": {"l40-node": {"nvidia.com/gpu.product=NVIDIA-L40": 8}},
	}, nil
}

func (w *concurrentAdmissionWorkload) GetNodeResourceCapacity(context.Context) (map[string]map[string]map[string]int64, error) {
	return map[string]map[string]map[string]int64{
		"cluster-a": {"l40-node": {
			domain.NodeResourceCPUMillicores: 64000,
			domain.NodeResourceMemoryBytes:   1 << 40,
			domain.NodeResourceStorageBytes:  1 << 40,
		}},
	}, nil
}

func (w *concurrentAdmissionWorkload) GetNodeLabels(context.Context) (map[string]map[string]map[string]string, error) {
	return map[string]map[string]map[string]string{"cluster-a": {"l40-node": {"nvidia.com/gpu.product": "NVIDIA-L40"}}}, nil
}

func TestConcurrentOperatorAdmissionsDoNotExceedClusterCapacity(t *testing.T) {
	store := &concurrentAdmissionStore{experiments: make(map[string]*domain.Experiment)}
	for _, id := range []string{"job-1", "job-2", "job-3"} {
		store.experiments[id] = &domain.Experiment{
			ID:               id,
			Status:           domain.StatusQueued,
			AcceleratorType:  "nvidia.com/gpu.product=NVIDIA-L40",
			AcceleratorCount: 4,
			Job:              domain.JobSpec{AcceleratorType: "nvidia.com/gpu.product=NVIDIA-L40"},
		}
	}
	workload := &concurrentAdmissionWorkload{store: store}
	handler := NewHandler(NewService(store, nil, workload, nil, nil, noopSettler{}, "http://metrics.invalid", zap.NewNop()).WithLoop(noopLoop{}))

	var wg sync.WaitGroup
	results := make(chan error, 3)
	for id := range store.experiments {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			_, err := handler.admit(context.Background(), id, "cluster-a")
			results <- err
		}(id)
	}
	wg.Wait()
	close(results)

	succeeded := 0
	for err := range results {
		if err == nil {
			succeeded++
		}
	}
	if succeeded != 2 {
		t.Fatalf("successful admissions = %d, want 2", succeeded)
	}
	if store.claimed != 8 {
		t.Fatalf("claimed accelerator capacity = %d, want 8", store.claimed)
	}
}

type noopLoop struct{}

func (noopLoop) Trigger() {}
