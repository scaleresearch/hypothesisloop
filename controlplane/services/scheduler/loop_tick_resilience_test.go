package scheduler

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// resilienceStore answers only what the prefilter and admission passes actually call. Anything
// else is left to the embedded nil interface, so a test that starts depending on more fails
// loudly instead of quietly passing.
type resilienceStore struct {
	LoopStore
	queued []*domain.Experiment
	// missingPE names a platform experiment that no longer exists — the corrupt row this test is
	// about.
	missingPE      string
	notAdmitted    map[string]string
	listRunningErr error
}

func (s *resilienceStore) ListQueuedExperiments(context.Context) ([]*domain.Experiment, error) {
	return s.queued, nil
}

func (s *resilienceStore) ListRunningExperiments(context.Context) ([]*domain.Experiment, error) {
	return nil, s.listRunningErr
}

func (s *resilienceStore) GetPlatformExperiment(_ context.Context, id string) (*domain.PlatformExperiment, error) {
	if id == s.missingPE {
		return nil, nil // the row was deleted; this is what the store reports
	}
	return &domain.PlatformExperiment{ID: id}, nil
}

func (s *resilienceStore) IsAgentCut(context.Context, string, string) (bool, error) {
	return false, nil
}

func (s *resilienceStore) HasUnsummarizedCompleted(context.Context, string, string) (bool, error) {
	return false, nil
}

func (s *resilienceStore) UpdateNotAdmittedReason(_ context.Context, id, reason string) error {
	if s.notAdmitted == nil {
		s.notAdmitted = map[string]string{}
	}
	s.notAdmitted[id] = reason
	return nil
}

type resilienceQuota struct{ LoopQuotaStore }

func (resilienceQuota) GetAgentQuota(context.Context, string, string) (*domain.AgentQuota, error) {
	return &domain.AgentQuota{}, nil
}

// resilienceWorkload reports a cluster with no free capacity, so every job is skipped rather than
// submitted — this test is about the prefilter surviving, not about placement.
type resilienceWorkload struct{ LoopWorkloadClient }

func (resilienceWorkload) GetFlavorCapacity(context.Context) (map[string]domain.Footprint, map[string]domain.Footprint, error) {
	return map[string]domain.Footprint{"c1": domain.NewFootprint()},
		map[string]domain.Footprint{"c1": domain.NewFootprint()}, nil
}

func (resilienceWorkload) GetAcceleratorCapacityByNode(context.Context) (map[string]map[string]map[string]int64, error) {
	return map[string]map[string]map[string]int64{"c1": {}}, nil
}

func (resilienceWorkload) GetNodeLabels(context.Context) (map[string]map[string]map[string]string, error) {
	return map[string]map[string]map[string]string{"c1": {}}, nil
}

func (resilienceWorkload) GetTotalCapacity(context.Context) (map[string]domain.Footprint, error) {
	return map[string]domain.Footprint{}, nil
}

func resilienceLoop(store LoopStore) *Loop {
	l := NewLoop(store, resilienceQuota{}, resilienceWorkload{}, zap.NewNop())
	l.evictor = noopEvictor{}
	l.disbalanceTolerance = DefaultDisbalanceTolerance
	return l
}

type noopEvictor struct{}

func (noopEvictor) EvictExperiment(context.Context, string, domain.EvictionReason) error { return nil }

func queuedJob(id, peID string, tier domain.CapacityTier) *domain.Experiment {
	return &domain.Experiment{
		ID:                   id,
		AgentID:              "agent-1",
		PlatformExperimentID: peID,
		ClusterName:          "c1",
		CapacityTier:         tier,
		AcceleratorType:      "nvidia.com/gpu.product=NVIDIA-L40",
		AcceleratorCount:     1,
	}
}

// A QUEUED job pointing at a deleted platform experiment is re-read on every tick, forever. If it
// aborts the pass, no job anywhere on the platform is ever admitted again — the failure mode
// important.md #19 exists to forbid. The corrupt row must be skipped, every other job must still
// be considered, and the tick must still report the error rather than swallow it.
func TestTickSkipsACorruptRowInsteadOfAbandoningEveryOtherJob(t *testing.T) {
	store := &resilienceStore{
		queued: []*domain.Experiment{
			queuedJob("exp-corrupt", "pe-deleted", domain.CapacityGuaranteed),
			queuedJob("exp-healthy", "pe-live", domain.CapacityGuaranteed),
		},
		missingPE: "pe-deleted",
	}
	l := resilienceLoop(store)

	err := l.tick(context.Background())
	if err == nil {
		t.Fatal("tick reported success despite a corrupt row — the failure must still surface")
	}
	if !strings.Contains(err.Error(), "exp-corrupt") {
		t.Errorf("joined error %q does not name the offending experiment", err)
	}
	if _, considered := store.notAdmitted["exp-healthy"]; !considered {
		t.Error("the healthy job was never considered — one corrupt row abandoned the pass")
	}
	if _, ok := store.notAdmitted["exp-corrupt"]; ok {
		t.Error("the corrupt row was given a not-admitted reason; it should be skipped entirely")
	}
}

// The same guarantee on the path where no burst work remains: an early return there once dropped
// every collected error on the floor, so a permanently corrupt row became invisible.
func TestTickStillReportsCollectedErrorsWhenNoBurstJobsRemain(t *testing.T) {
	store := &resilienceStore{
		queued:    []*domain.Experiment{queuedJob("exp-corrupt", "pe-deleted", domain.CapacityGuaranteed)},
		missingPE: "pe-deleted",
	}
	l := resilienceLoop(store)

	err := l.tick(context.Background())
	if err == nil {
		t.Fatal("tick returned nil with no burst jobs queued, discarding the collected failure")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error %q does not describe the missing platform experiment", err)
	}
}

// A tick with nothing wrong must stay quiet: errors.Join over an empty slice is nil, and a tick
// that reported a spurious error every pass would train its reader to ignore the log.
func TestTickReportsNoErrorWhenEveryRowIsHealthy(t *testing.T) {
	store := &resilienceStore{
		queued: []*domain.Experiment{queuedJob("exp-a", "pe-live", domain.CapacityGuaranteed)},
	}
	l := resilienceLoop(store)

	if err := l.tick(context.Background()); err != nil {
		t.Fatalf("healthy tick returned %v, want nil", err)
	}
}

// Both remedies for a job that does not fit — preemption and the disbalance evictor — reason
// about what is currently running. When that set cannot be read, neither has a basis, and the
// tick must terminate nothing rather than act on state it could not see. It must also survive:
// an unreadable running set is a transient store failure, not grounds to stop admitting.
func TestTickTerminatesNothingWhenTheRunningSetCannotBeRead(t *testing.T) {
	store := &resilienceStore{
		queued:         []*domain.Experiment{queuedJob("exp-a", "pe-live", domain.CapacityGuaranteed)},
		listRunningErr: errors.New("metrics store unreachable"),
	}
	l := resilienceLoop(store)
	l.evictor = panickingEvictor{t}

	err := l.tick(context.Background())
	if err == nil {
		t.Fatal("tick hid an unreadable running set entirely")
	}
	if !strings.Contains(err.Error(), "metrics store unreachable") {
		t.Errorf("error %q does not describe the underlying failure", err)
	}
}

type panickingEvictor struct{ t *testing.T }

func (p panickingEvictor) EvictExperiment(context.Context, string, domain.EvictionReason) error {
	p.t.Fatal("disbalance eviction ran after preemption failed to establish a plan")
	return nil
}
