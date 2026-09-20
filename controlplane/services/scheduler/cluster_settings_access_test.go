package scheduler

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// registeredClustersStore holds the registered clusters by name, each with the id it is bound to.
type registeredClustersStore struct {
	Store
	clusters map[string]domain.Cluster
}

func (s *registeredClustersStore) GetCluster(_ context.Context, name string) (*domain.Cluster, error) {
	if c, ok := s.clusters[name]; ok {
		return &c, nil
	}
	return nil, nil
}

func (s *registeredClustersStore) ClusterByReportedID(_ context.Context, reportedID string) (*domain.Cluster, error) {
	for _, c := range s.clusters {
		if c.ReportedID != "" && c.ReportedID == reportedID {
			return &c, nil
		}
	}
	return nil, nil
}

// reportingAnyIDWorkload is every connected cluster claiming the same id, the one bound to
// cluster-b: what a runtime states about itself decides nothing.
type reportingAnyIDWorkload struct{ concurrentAdmissionWorkload }

func (reportingAnyIDWorkload) GetClusterIDs(context.Context) (map[string]string, error) {
	return map[string]string{"cluster-a": "uid-b", "cluster-b": "uid-b"}, nil
}

// Settings are keyed by the id a cluster reports and a cluster is owned by name, so the check
// goes through the registered cluster the platform bound the id to.
func TestClusterSettingsAreReachedThroughTheClusterTheIDIsBoundTo(t *testing.T) {
	store := &registeredClustersStore{clusters: map[string]domain.Cluster{
		"cluster-a": {Name: "cluster-a", OrgID: "org-a", ReportedID: "uid-a"},
		"cluster-b": {Name: "cluster-b", OrgID: "org-b", ReportedID: "uid-b"},
		"cluster-c": {Name: "cluster-c", OrgID: "org-a"},
	}}
	handler := NewHandler(NewService(store, nil, &reportingAnyIDWorkload{}, nil, nil, noopSettler{}, testGraceLimits, "http://metrics.invalid", zap.NewNop()).WithLoop(noopLoop{}))
	access := auth.NewAccess(authtest.Placement{
		Clusters:   map[string]string{"cluster-a": "org-a", "cluster-b": "org-b", "cluster-c": "org-a"},
		ClusterIDs: map[string]string{"uid-a": "cluster-a", "uid-b": "cluster-b"},
	})

	inA := []string{"org-a"}
	userOfA := auth.Principal{Kind: auth.KindUser, OrgIDs: inA}
	staff := auth.Principal{Kind: auth.KindStaff}
	for _, tc := range []struct {
		name      string
		caller    auth.Principal
		clusterID string
		admitted  bool
	}{
		{"owner, by the id its cluster is bound to", userOfA, "uid-a", true},
		{"an org-wide key of the owner", auth.Principal{Kind: auth.KindKey, OrgIDs: inA}, "uid-a", true},
		{"another org's cluster, though the caller's own runtime reports its id", userOfA, "uid-b", false},
		{"its own cluster's name used as an id", userOfA, "cluster-a", false},
		{"an id bound to no cluster", userOfA, "uid-gone", false},
		{"the empty id of a cluster that never reported", userOfA, "", false},
		{"a key bound to one project of the owner", auth.Principal{Kind: auth.KindKey, OrgIDs: inA, ProjectID: "prj-a"}, "uid-a", false},
		{"a coordinator of the owner's platform experiment", auth.Principal{Kind: auth.KindCoordinator, OrgIDs: inA, ProjectID: "prj-a", PlatformExperimentID: "pe-a"}, "uid-a", false},
		{"staff, any cluster", staff, "uid-b", true},
		{"staff, an id bound to no cluster", staff, "uid-gone", false},
		{"staff, an id that is a path", staff, "uid-a/../uid-b", false},
	} {
		err := handler.clusterAccess(auth.WithPrincipal(context.Background(), tc.caller), access, tc.clusterID)
		var refusal *auth.Error
		if tc.admitted && err != nil {
			t.Errorf("%s: got = %v, want admitted", tc.name, err)
		}
		if !tc.admitted && (!errors.As(err, &refusal) || refusal.GetStatus() != 404) {
			t.Errorf("%s: got = %v, want 404", tc.name, err)
		}
	}
}

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
				Job: domain.JobSpec{AcceleratorType: l40},
			}}}
			store := &hostingStore{concurrentAdmissionStore: admissions, clusters: map[string]domain.Cluster{tc.cluster.Name: tc.cluster}}
			workload := &concurrentAdmissionWorkload{store: admissions}
			handler := NewHandler(NewService(store, nil, workload, nil, nil, noopSettler{}, testGraceLimits, "http://metrics.invalid", zap.NewNop()).WithLoop(noopLoop{}))

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
