package registry

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

func jobOn(id, hypothesisID string, status domain.ExperimentStatus) *domain.Experiment {
	return &domain.Experiment{ID: id, HypothesisID: hypothesisID, PlatformExperimentID: "pe-1", Status: status}
}

// Six hosted agents with the same evidence, one run each and no measured baseline, called their
// hypotheses confirmed, inconclusive and open, and a summary repeated a win over a baseline
// nobody ran. A verdict is a claim about a difference, so the record has to hold both sides of
// it and a repeat, or the agent's own words for why it holds without.
func TestAConfirmedOrRefutedVerdictNeedsARepeatedRunAndACompletedControl(t *testing.T) {
	ctx := context.Background()
	for _, status := range []domain.HypothesisStatus{domain.HypothesisConfirmed, domain.HypothesisRefuted} {
		store := newPoolStore()
		svc := poolService(store)
		h, _, _, err := svc.RegisterHypothesis(ctx, "agent-1", agentActor("agent-1"), "pe-1", "a wider layer helps "+string(status), buildOnRoot, nil)
		if err != nil {
			t.Fatal(err)
		}
		other := &domain.Experiment{ID: "job-elsewhere", PlatformExperimentID: "pe-2", Status: domain.StatusCompleted}
		store.jobs = []*domain.Experiment{jobOn("job-1", h.ID, domain.StatusCompleted), other,
			jobOn("job-control-running", "", domain.StatusRunning), jobOn("job-control", "", domain.StatusCompleted)}

		refusals := map[string]struct {
			verdict Verdict
			says    []string
		}{
			"one run, no control":       {Verdict{Status: status}, []string{"no control is named", "against", "inconclusive"}},
			"one run with a control":    {Verdict{Status: status, Against: "job-control"}, []string{"needs 2 completed jobs", "has 1 (job-1)", "run it again"}},
			"its only run as control":   {Verdict{Status: status, Against: "job-1"}, []string{"has 0 (none)"}},
			"a control still running":   {Verdict{Status: status, Against: "job-control-running"}, []string{"is RUNNING"}},
			"a control of another one":  {Verdict{Status: status, Against: "job-elsewhere"}, []string{"no job of this platform experiment"}},
			"a control that is no job":  {Verdict{Status: status, Against: "job-nowhere"}, []string{"no job of this platform experiment"}},
			"a reason that is one word": {Verdict{Status: status, Despite: "obvious"}, []string{"say in a sentence"}},
		}
		for name, c := range refusals {
			_, err := svc.SetHypothesisStatus(ctx, h.ID, "agent-1", agentActor("agent-1"), c.verdict)
			var unsupported *UnsupportedVerdictError
			if !errors.As(err, &unsupported) {
				t.Fatalf("%s, %s: err = %v, want the verdict refused", status, name, err)
			}
			for _, want := range c.says {
				if !strings.Contains(unsupported.Detail, want) {
					t.Errorf("%s, %s: refusal %q does not say %q", status, name, unsupported.Detail, want)
				}
			}
			var answered huma.StatusError
			if !errors.As(hypothesisWriteError(err), &answered) || answered.GetStatus() != http.StatusConflict {
				t.Errorf("%s, %s: answered %v, want 409", status, name, hypothesisWriteError(err))
			}
		}
		if got, _ := svc.GetHypothesis(ctx, h.ID); got.Status != domain.HypothesisOpen || len(store.comments) != 0 {
			t.Fatalf("after the refusals the hypothesis is %s with notes %v", got.Status, store.comments)
		}

		// A failed second run is no repeat; a completed one is.
		store.jobs = append(store.jobs, jobOn("job-2", h.ID, domain.StatusFailed))
		if _, err := svc.SetHypothesisStatus(ctx, h.ID, "agent-1", agentActor("agent-1"), Verdict{Status: status, Against: "job-control"}); err == nil {
			t.Fatalf("%s with one completed and one failed run was accepted", status)
		}
		store.jobs = append(store.jobs, jobOn("job-3", h.ID, domain.StatusCompleted))
		settled, err := svc.SetHypothesisStatus(ctx, h.ID, "agent-1", agentActor("agent-1"), Verdict{Status: status, Against: "job-control"})
		if err != nil || settled.Status != status {
			t.Fatalf("%s with two completed runs and a control: %v, %+v", status, err, settled)
		}
		if len(store.comments) != 1 || !strings.Contains(store.comments[0], "against control job-control") {
			t.Fatalf("the basis of the verdict was noted as %v", store.comments)
		}
	}
}

func TestAVerdictWithoutTheEvidenceStandsOnAStatedReasonWhichIsKept(t *testing.T) {
	ctx := context.Background()
	store := newPoolStore()
	svc := poolService(store)
	h, _, _, err := svc.RegisterHypothesis(ctx, "agent-1", agentActor("agent-1"), "pe-1", "lr 10 trains", buildOnRoot, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetHypothesisStatus(ctx, h.ID, "agent-1", agentActor("agent-1"), Verdict{Status: domain.HypothesisRefuted, Despite: enoughSaid}); err != nil {
		t.Fatal(err)
	}
	if len(store.comments) != 1 || !strings.Contains(store.comments[0], enoughSaid) {
		t.Fatalf("the reason was noted as %v", store.comments)
	}
}

// What needs no evidence: saying a hypothesis is open or inconclusive, and saying again what it
// already is.
func TestOpenAndInconclusiveNeedNoEvidence(t *testing.T) {
	ctx := context.Background()
	store := newPoolStore()
	svc := poolService(store)
	h, _, _, err := svc.RegisterHypothesis(ctx, "agent-1", agentActor("agent-1"), "pe-1", "dropout helps", buildOnRoot, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []domain.HypothesisStatus{domain.HypothesisInconclusive, domain.HypothesisOpen} {
		if _, err := svc.SetHypothesisStatus(ctx, h.ID, "agent-1", agentActor("agent-1"), Verdict{Status: status}); err != nil {
			t.Fatalf("%s: %v", status, err)
		}
	}
	if len(store.comments) != 0 {
		t.Fatalf("notes written for verdicts that claim nothing: %v", store.comments)
	}
}
