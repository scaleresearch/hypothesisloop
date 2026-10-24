package platformexperiments

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/metricsdb"
)

func stagedPE(currentStage int) *domain.PlatformExperiment {
	started := time.Date(2026, 10, 6, 21, 0, 0, 0, time.UTC)
	return &domain.PlatformExperiment{
		ID: "pe-1", Status: domain.PlatformExpRunning, CreatedAt: started.Add(-time.Hour),
		StartedAt: started, EndsAt: started.Add(45 * time.Minute), BudgetAcceleratorHours: 6,
		Stages:       []domain.Stage{{LengthPct: 40, EvictPct: 0}, {LengthPct: 60, EvictPct: 0}},
		CurrentStage: currentStage,
	}
}

// Two executors stopped believing their quota was spent, minutes before a boundary raised it.
func TestTheNextStageIsWhenTheBoundaryFallsAndWhatItGivesEachExecutorStanding(t *testing.T) {
	pe := stagedPE(1)
	startsAt, credit, ok := nextStageRelease(pe, 6, 2)
	if !ok {
		t.Fatal("a running platform experiment in its first of two stages has a boundary ahead")
	}
	if want := pe.StartedAt.Add(18 * time.Minute); !startsAt.Equal(want) {
		t.Errorf("starts at %s, want %s: 40%% of 45 minutes", startsAt, want)
	}
	if credit != 1.8 {
		t.Errorf("credit = %v, want 1.8: 60%% of 6 hours over 2 executors", credit)
	}

	if _, _, ok := nextStageRelease(stagedPE(2), 6, 2); ok {
		t.Error("the last stage has no boundary ahead")
	}
	open := stagedPE(1)
	open.Status = domain.PlatformExpOpen
	if _, _, ok := nextStageRelease(open, 6, 2); ok {
		t.Error("an open platform experiment has no boundary ahead")
	}
	if _, _, ok := nextStageRelease(pe, 6, 0); ok {
		t.Error("with no executor standing nobody is given anything")
	}
}

type stagedStore struct {
	grantedStore
	pe *domain.PlatformExperiment
}

func (f *stagedStore) GetPlatformExperiment(context.Context, string) (*domain.PlatformExperiment, error) {
	return f.pe, nil
}

func (*stagedStore) ListSignupsByRole(_ context.Context, _ string, role domain.SignupRole) ([]string, error) {
	if role == domain.SignupRoleExecutor {
		return []string{"a", "b", "cut"}, nil
	}
	return nil, nil
}

func (*stagedStore) ListCutAgents(context.Context, string) ([]domain.AgentCut, error) {
	return []domain.AgentCut{{AgentID: "cut", StageIndex: 1}}, nil
}

func TestAQuotaSaysWhatTheNextStageBringsAndNothingInTheLastOne(t *testing.T) {
	metrics := aliveServer(t, time.Now().UTC(), 30*time.Second, 2*time.Minute)
	defer metrics.Close()
	store := &stagedStore{pe: stagedPE(1)}
	svc := NewService(store, domain.QuotaConfig{}, zap.NewNop(), metricsdb.New(metrics.URL), 3*time.Minute)

	view, err := svc.QuotaView(context.Background(), "a", "pe-1")
	if err != nil {
		t.Fatal(err)
	}
	if view.NextStageStartsAt == nil || !view.NextStageStartsAt.Equal(store.pe.StartedAt.Add(18*time.Minute)) {
		t.Errorf("next_stage_starts_at = %v, want 18 minutes into the run", view.NextStageStartsAt)
	}
	if view.NextStageShare == nil || *view.NextStageShare != 2.8 {
		t.Errorf("next_stage_share_accelerator_hours = %v, want 2.8: the share of 1 and 60%% of 6 over the 2 executors not cut", view.NextStageShare)
	}

	view, err = svc.QuotaView(context.Background(), "cut", "pe-1")
	if err != nil {
		t.Fatal(err)
	}
	if view.NextStageStartsAt != nil || view.NextStageShare != nil {
		t.Errorf("a cut agent is promised %v at %v, want nothing", view.NextStageShare, view.NextStageStartsAt)
	}

	store.pe = stagedPE(2)
	view, err = svc.QuotaView(context.Background(), "a", "pe-1")
	if err != nil {
		t.Fatal(err)
	}
	if view.NextStageStartsAt != nil || view.NextStageShare != nil {
		t.Errorf("the last stage promises %v at %v, want nothing", view.NextStageShare, view.NextStageStartsAt)
	}
}
