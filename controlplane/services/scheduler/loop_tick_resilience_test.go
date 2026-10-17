package scheduler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
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
type resilienceWorkload struct{ ClusterCapacity }

func (resilienceWorkload) GetFlavorCapacity(context.Context) (map[string]domain.Footprint, map[string]domain.Footprint, error) {
	return map[string]domain.Footprint{"c1": domain.NewFootprint()},
		map[string]domain.Footprint{"c1": domain.NewFootprint()}, nil
}

func (resilienceWorkload) GetAcceleratorCapacityByNode(context.Context) (map[string]map[string]map[string]int64, error) {
	return map[string]map[string]map[string]int64{"c1": {}}, nil
}

func (resilienceWorkload) GetNodeResourceCapacity(context.Context) (map[string]map[string]map[string]int64, error) {
	return map[string]map[string]map[string]int64{"c1": {}}, nil
}

func (resilienceWorkload) GetNodeTotalCapacity(context.Context) (map[string]map[string]map[string]int64, error) {
	return map[string]map[string]map[string]int64{"c1": {}}, nil
}

func (resilienceWorkload) GetNodeLabels(context.Context) (map[string]map[string]map[string]string, error) {
	return map[string]map[string]map[string]string{"c1": {}}, nil
}

func (resilienceWorkload) GetMultiNodeCapability(context.Context) (map[string]bool, error) {
	return map[string]bool{"c1": true}, nil
}

func (resilienceWorkload) GetTotalCapacity(context.Context) (map[string]domain.Footprint, error) {
	return map[string]domain.Footprint{}, nil
}

func resilienceLoop(store LoopStore) *Loop {
	l := NewLoop(store, resilienceQuota{}, resilienceWorkload{}, testGraceLimits, zap.NewNop())
	l.observed = fakeObserved{}
	l.evictor = noopEvictor{}
	l.disbalanceTolerance = testDisbalanceTolerance
	l.reprioritizer = noopReprioritizer{}
	return l
}

type noopReprioritizer struct{}

func (noopReprioritizer) RePrioritize(context.Context) error { return nil }

type noopEvictor struct{}

func (noopEvictor) EvictExperiment(context.Context, string, domain.EvictionReason) error { return nil }

func queuedJob(id, peID string, tier domain.CapacityTier) *domain.Experiment {
	return &domain.Experiment{
		ID:                   id,
		AgentID:              "agent-1",
		PlatformExperimentID: peID,
		ClusterName:          "c1",
		CapacityTier:         tier,
		AcceleratorType:      "nvidia.com/gpu.product=nvidia-l40",
		AcceleratorCount:     1,
		Job:                  domain.JobSpec{Image: "registry.test/job:1"},
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

type failingAgentQuota struct {
	LoopQuotaStore
	agent string
}

func (q failingAgentQuota) GetAgentQuota(_ context.Context, agentID, _ string) (*domain.AgentQuota, error) {
	if agentID == q.agent {
		return nil, errors.New("quota row unreadable")
	}
	return &domain.AgentQuota{}, nil
}

func TestTickSkipsAnAgentWhoseQuotaCannotBeRead(t *testing.T) {
	for _, tier := range []domain.CapacityTier{domain.CapacityGuaranteed, domain.CapacityBurst} {
		broken := queuedJob("exp-broken", "pe-live", tier)
		broken.AgentID = "agent-broken"
		store := &resilienceStore{queued: []*domain.Experiment{broken, queuedJob("exp-healthy", "pe-live", tier)}}
		l := resilienceLoop(store)
		l.quota = failingAgentQuota{agent: "agent-broken"}

		err := l.tick(context.Background())
		if err == nil || !strings.Contains(err.Error(), "exp-broken") {
			t.Errorf("%s: tick error = %v, want it to name the skipped job", tier, err)
		}
		if _, considered := store.notAdmitted["exp-healthy"]; !considered {
			t.Errorf("%s: one agent's unreadable quota kept another agent's job from being considered", tier)
		}
		if _, ok := store.notAdmitted["exp-broken"]; ok {
			t.Errorf("%s: the job with no readable quota was judged anyway", tier)
		}
	}
}

type skippedClusterWorkload struct{ resilienceWorkload }

func (w skippedClusterWorkload) GetFlavorCapacity(ctx context.Context) (map[string]domain.Footprint, map[string]domain.Footprint, error) {
	available, desiredFree, _ := w.resilienceWorkload.GetFlavorCapacity(ctx)
	return available, desiredFree, InconsistentSnapshots{"c2": errors.New("available capacity exceeds total")}
}

func TestTickLogsASkippedClusterWithItsReasonAndContinues(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	store := &resilienceStore{queued: []*domain.Experiment{queuedJob("exp-a", "pe-live", domain.CapacityGuaranteed)}}
	l := resilienceLoop(store)
	l.clusters = skippedClusterWorkload{}
	l.logger = zap.New(core)

	if err := l.tick(context.Background()); err != nil {
		t.Fatalf("tick() = %v, want the other cluster still served", err)
	}
	if _, considered := store.notAdmitted["exp-a"]; !considered {
		t.Fatal("the job on the healthy cluster was never considered")
	}
	entries := logs.FilterField(zap.String("cluster", "c2")).All()
	if len(entries) != 1 || !strings.Contains(fmt.Sprint(entries[0].ContextMap()["error"]), "exceeds total") {
		t.Fatalf("skipped cluster log = %v, want one entry carrying the inconsistency", entries)
	}
}

func (*resilienceStore) ListCapacityClaimedExperiments(context.Context) ([]*domain.Experiment, error) {
	return nil, nil
}

func (*resilienceStore) MarkHeldJobs(context.Context, time.Duration) error { return nil }

type heldStore struct {
	resilienceStore
	// reasonsAtMark is what each job waited for when the held jobs were marked.
	reasonsAtMark map[string]string
	minimum       time.Duration
	marks         int
}

func (s *heldStore) MarkHeldJobs(_ context.Context, minimum time.Duration) error {
	s.marks++
	s.minimum = minimum
	s.reasonsAtMark = map[string]string{}
	for id, reason := range s.notAdmitted {
		s.reasonsAtMark[id] = reason
	}
	return nil
}

// A job told it is held is told why by the reason this pass gave it, so the pass marks the held
// jobs once, after it judged every one, with the platform's minimum wait.
func TestATickMarksTheHeldJobsOnceAfterItJudgedTheQueue(t *testing.T) {
	blocked := queuedJob("exp-a", "pe-live", domain.CapacityGuaranteed)
	store := &heldStore{resilienceStore: resilienceStore{queued: []*domain.Experiment{blocked}}}
	loop := resilienceLoop(store).WithHeldJobNotice(5 * time.Minute)

	if err := loop.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.marks != 1 || store.minimum != 5*time.Minute {
		t.Fatalf("held jobs marked %d times with a minimum of %s, want once with 5m", store.marks, store.minimum)
	}
	if reason, _ := domain.SplitQueueReason(store.reasonsAtMark[blocked.ID]); reason != domain.NotAdmittedCapacityUnavailable {
		t.Errorf("at the mark the blocked job waited for %q, want the reason this pass gave it", store.reasonsAtMark[blocked.ID])
	}
}
