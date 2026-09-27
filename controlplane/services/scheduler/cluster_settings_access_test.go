package scheduler

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// Force-admission names the cluster, and the cluster still has to take the job: staff may reach
// every job and every cluster, and may not put one org's job on another org's cluster.
func TestForceAdmissionPlacesAJobOnlyOnAClusterThatHostsIt(t *testing.T) {
	const l40 = "nvidia.com/gpu.product=NVIDIA-L40"
	for _, tc := range []struct {
		name    string
		cluster domain.Cluster
		status  int
	}{
		{"the job's own org's cluster", domain.Cluster{Name: "cluster-a", OrgID: "org-a"}, 0},
		{"a shared cluster of another org", domain.Cluster{Name: "cluster-a", OrgID: "org-staff", Shared: true}, 0},
		{"another org's cluster", domain.Cluster{Name: "cluster-a", OrgID: "org-b"}, 409},
		{"a cluster nobody registered", domain.Cluster{Name: "elsewhere", OrgID: "org-a"}, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			admissions := &concurrentAdmissionStore{experiments: map[string]*domain.Experiment{"job-1": {
				ID: "job-1", OrgID: "org-a", Status: domain.StatusQueued, AcceleratorType: l40, AcceleratorCount: 4,
				Job: domain.JobSpec{Image: "registry.test/job:1", AcceleratorType: l40},
			}}}
			store := &hostingStore{concurrentAdmissionStore: admissions, clusters: map[string]domain.Cluster{tc.cluster.Name: tc.cluster}}
			workload := &concurrentAdmissionWorkload{store: admissions}
			handler := NewHandler(admissionsUnrecorded(NewService(store, nil, workload, nil, nil, testCodeHost{}, noopSettler{}, testGraceLimits, "http://metrics.invalid", zap.NewNop())).WithLoop(noopLoop{}))

			_, err := handler.admit(context.Background(), "job-1", "cluster-a")

			var refusal interface{ GetStatus() int }
			switch {
			case tc.status == 0 && (err != nil || admissions.experiments["job-1"].ClusterName != "cluster-a"):
				t.Fatalf("got = %v with the job on %q, want it admitted to cluster-a", err, admissions.experiments["job-1"].ClusterName)
			case tc.status != 0 && (!errors.As(err, &refusal) || refusal.GetStatus() != tc.status):
				t.Fatalf("got = %v, want status %d", err, tc.status)
			case tc.status != 0 && admissions.experiments["job-1"].Status != domain.StatusQueued:
				t.Fatal("a refused admission claimed the job")
			}
		})
	}
}

// hostingStore is the admission store with the clusters that are registered.
type hostingStore struct {
	*concurrentAdmissionStore
	clusters map[string]domain.Cluster
}

func (s *hostingStore) GetCluster(_ context.Context, name string) (*domain.Cluster, error) {
	if c, ok := s.clusters[name]; ok {
		return &c, nil
	}
	return nil, nil
}
