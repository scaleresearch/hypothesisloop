package controller

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// everyPEStagesStore holds a roster of running platform experiments and records which of them a
// reconcile pass actually advanced. Every StagesStore method the pass does not need is inherited
// from the embedded nil interface, so reaching one panics loudly instead of returning a zero
// value — including ListPlatformExperiments, the agent-facing paged list this loop must never
// read the roster through.
type everyPEStagesStore struct {
	StagesStore
	running  []*domain.PlatformExperiment
	advanced map[string]bool
}

func (f *everyPEStagesStore) ListPlatformExperimentsByStatus(ctx context.Context, status domain.PlatformExperimentStatus) ([]*domain.PlatformExperiment, error) {
	if status == domain.PlatformExpRunning {
		return f.running, nil
	}
	return nil, nil
}

func (f *everyPEStagesStore) ListCutAgents(ctx context.Context, platformExpID string) ([]domain.AgentCut, error) {
	return nil, nil
}

func (f *everyPEStagesStore) ListSignupsByRole(ctx context.Context, platformExpID string, role domain.SignupRole) ([]string, error) {
	return nil, nil
}

func (f *everyPEStagesStore) ListAgentQuotas(ctx context.Context, platformExpID string) ([]*domain.AgentQuota, error) {
	return nil, nil
}

func (f *everyPEStagesStore) AddDesiredQuotaUsage(ctx context.Context, platformExpID string, quotas []*domain.AgentQuota) error {
	return nil
}

func (f *everyPEStagesStore) AdvanceStage(ctx context.Context, platformExpID string, stageIndex int, cutAgentIDs, survivorIDs []string, dims []db.StageRedistribution) (bool, error) {
	f.advanced[platformExpID] = true
	return true, nil
}

// noRunningJobsStore is the controller's own Store for a pass with no running jobs: the stage
// ladder is the only thing left with work to do, which is exactly what this test is about.
type noRunningJobsStore struct {
	Store
}

func (f *noRunningJobsStore) ListRunningExperiments(ctx context.Context) ([]*domain.Experiment, error) {
	return nil, nil
}

// emptyVectorServer answers every metrics query with no samples — a platform experiment whose
// budget is untouched. The ladders below cross their boundary on the wall clock instead.
func emptyVectorServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
}

// A reconcile pass is a control loop: it acts on every running platform experiment or on none of
// them. Reading the roster through the agent-facing paged list capped it at that list's default
// page, so on a platform busy enough to hold more running experiments than one page, the ladders
// beyond it silently stopped advancing — no boundary, no cut, no error, nothing in the API to say
// why. The count here is deliberately just past that default.
func TestReconcileAdvancesTheLadderOfEveryRunningPlatformExperimentNotOnlyTheFirstPage(t *testing.T) {
	srv := emptyVectorServer(t)
	defer srv.Close()

	// Both stages evict nobody, so each boundary is a pure advance and no ranking is involved:
	// what is being measured is which platform experiments the pass reached at all.
	now := time.Now().UTC()
	stages := []domain.Stage{{LengthPct: 50, EvictPct: 0}, {LengthPct: 50, EvictPct: 0}}
	store := &everyPEStagesStore{advanced: map[string]bool{}}
	for i := 0; i < 25; i++ {
		store.running = append(store.running, &domain.PlatformExperiment{
			ID:           fmt.Sprintf("pe-%02d", i),
			Status:       domain.PlatformExpRunning,
			Metrics:      []domain.MetricDefinition{{Key: "val_accuracy", Direction: "maximize"}},
			Stages:       stages,
			CurrentStage: 1,
			CreatedAt:    now.Add(-2 * time.Hour),
			// The window has fully elapsed, so every one of these is past its first boundary.
			StartsAt: now.Add(-2 * time.Hour),
			EndsAt:   now.Add(-time.Hour),
		})
	}

	c := New(&noRunningJobsStore{}, nil, zap.NewNop()).WithStagesStore(store, srv.URL)
	if err := c.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile err = %v, want nil", err)
	}

	for _, pe := range store.running {
		if !store.advanced[pe.ID] {
			t.Fatalf("%s is past its first boundary and was never advanced; only %d of %d ladders moved",
				pe.ID, len(store.advanced), len(store.running))
		}
	}
}
