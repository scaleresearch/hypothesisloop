package scheduler

import (
	"context"
	"testing"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

type lifecycleStore struct {
	transitioned bool
}

func (*lifecycleStore) ListExperimentsWithStatus(context.Context, domain.ExperimentStatus) ([]*domain.Experiment, error) {
	return nil, nil
}
func (*lifecycleStore) UpdateExperimentStatus(context.Context, string, domain.ExperimentStatus) error {
	return nil
}
func (*lifecycleStore) MarkStarted(context.Context, string) (bool, error) { return true, nil }
func (s *lifecycleStore) TransitionStatus(context.Context, string, domain.ExperimentStatus, domain.ExperimentStatus) (bool, error) {
	s.transitioned = true
	return true, nil
}
func (s *lifecycleStore) TransitionStatusFromNonTerminal(context.Context, string, domain.ExperimentStatus) (bool, error) {
	s.transitioned = true
	return true, nil
}
func (s *lifecycleStore) TransitionTerminal(context.Context, string, domain.ExperimentStatus, domain.ExperimentStatus, string) (bool, error) {
	s.transitioned = true
	return true, nil
}
func (*lifecycleStore) UpdateEvictionReason(context.Context, string, string) error { return nil }
func (*lifecycleStore) MarkQuotaSettled(context.Context, string) error             { return nil }

func TestTerminalObservationRequiresMetricsBeforeLifecycleTransition(t *testing.T) {
	store := &lifecycleStore{}
	w := NewJobWatcher(store, nil, zap.NewNop())
	exp := &domain.Experiment{ID: "exp-1", Status: domain.StatusSubmitted}

	if err := w.onFinished(context.Background(), exp, true); err == nil {
		t.Fatal("onFinished accepted a terminal observation without authoritative metrics")
	}
	if store.transitioned {
		t.Fatal("onFinished changed PostgreSQL desired state after metrics resolution failed")
	}
}
