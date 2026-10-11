package scheduler

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

type repeatStore struct {
	poolOnlyStore
	completed []*domain.Experiment
}

func (s repeatStore) ListExperiments(context.Context, domain.ExperimentFilter) ([]*domain.Experiment, error) {
	return s.completed, nil
}

type buildsOn struct {
	oneHypothesis
	parent string
}

func (b buildsOn) Links(context.Context, string) (domain.HypothesisLinks, error) {
	return domain.HypothesisLinks{Out: []domain.Link{{Type: domain.LinkBuildsOn, HypothesisID: b.parent}}}, nil
}

func repeatJob(id, hypothesis, commit string, env map[string]string) *domain.Experiment {
	return &domain.Experiment{ID: id, HypothesisID: hypothesis, CodeRef: "https://example.com/r.git@" + commit, Job: domain.JobSpec{Env: env}}
}

func refusalReason(t *testing.T, err error) string {
	t.Helper()
	var refused *AdmissionError
	if err == nil {
		return ""
	}
	if !errors.As(err, &refused) {
		t.Fatalf("got %v, want an AdmissionError or nil", err)
	}
	return refused.Reason
}

func TestAJobThatRunsWhatItsHypothesisBuildsOnUnchangedIsRefused(t *testing.T) {
	pe := &domain.PlatformExperiment{ID: "pe-1", BaselineJobID: "job-base"}
	base := repeatJob("job-base", "pe-1", "aaaa", map[string]string{"K": "1"})
	parent := repeatJob("job-parent", "h-parent", "bbbb", nil)
	cases := []struct {
		name   string
		parent string
		job    *domain.Experiment
		want   string
	}{
		{"root, same commit and env", "pe-1", repeatJob("j", "h-1", "aaaa", map[string]string{"K": "1"}), ReasonRunsNoChange},
		{"root, different commit", "pe-1", repeatJob("j", "h-1", "cccc", map[string]string{"K": "1"}), ""},
		{"root, different env", "pe-1", repeatJob("j", "h-1", "aaaa", map[string]string{"K": "2"}), ""},
		{"parent hypothesis, same commit and env", "h-parent", repeatJob("j", "h-1", "bbbb", nil), ReasonRunsNoChange},
		{"parent hypothesis, empty env equals none", "h-parent", repeatJob("j", "h-1", "bbbb", map[string]string{}), ReasonRunsNoChange},
		{"parent hypothesis, different env", "h-parent", repeatJob("j", "h-1", "bbbb", map[string]string{"LABEL": "x"}), ""},
	}
	for _, c := range cases {
		s := &Service{store: repeatStore{completed: []*domain.Experiment{base, parent}}, hypotheses: buildsOn{parent: c.parent}}
		err := s.refuseRepeats(context.Background(), pe, c.job)
		if got := refusalReason(t, err); got != c.want {
			t.Errorf("%s: reason %q, want %q (%v)", c.name, got, c.want, err)
		}
		if c.want != "" && !strings.Contains(err.Error(), "builds on unchanged") {
			t.Errorf("%s: message %q does not say the job runs the base", c.name, err)
		}
	}
}

func TestADeterministicRunRefusesWhatACompletedJobAlreadyMeasured(t *testing.T) {
	done := repeatJob("job-done", "h-other", "dddd", map[string]string{"K": "1"})
	deterministic := []domain.MetricDefinition{{Key: "bytes", Direction: "minimize", Deterministic: true}}
	timing := []domain.MetricDefinition{{Key: "bytes", Direction: "minimize", Deterministic: true}, {Key: "secs", Direction: "minimize"}}
	cases := []struct {
		name    string
		metrics []domain.MetricDefinition
		job     *domain.Experiment
		want    string
	}{
		{"deterministic, same commit and env", deterministic, repeatJob("j", "h-1", "dddd", map[string]string{"K": "1"}), ReasonAlreadyMeasured},
		{"deterministic, different commit", deterministic, repeatJob("j", "h-1", "eeee", map[string]string{"K": "1"}), ""},
		{"deterministic, different env", deterministic, repeatJob("j", "h-1", "dddd", map[string]string{"K": "2"}), ""},
		{"a ranking metric is not deterministic", timing, repeatJob("j", "h-1", "dddd", map[string]string{"K": "1"}), ""},
	}
	for _, c := range cases {
		pe := &domain.PlatformExperiment{ID: "pe-1", Metrics: c.metrics}
		s := &Service{store: repeatStore{completed: []*domain.Experiment{done}}, hypotheses: buildsOn{parent: "pe-1"}}
		err := s.refuseRepeats(context.Background(), pe, c.job)
		if got := refusalReason(t, err); got != c.want {
			t.Errorf("%s: reason %q, want %q (%v)", c.name, got, c.want, err)
		}
		if c.want != "" && !strings.Contains(err.Error(), "job job-done already measured") {
			t.Errorf("%s: message %q does not name the job", c.name, err)
		}
	}
}
