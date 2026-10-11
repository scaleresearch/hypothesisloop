package registry

import (
	"context"
	"errors"
	"fmt"
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
		h, _, _, _, err := svc.RegisterHypothesis(ctx, "agent-1", agentActor("agent-1"), "pe-1", "a wider layer helps "+string(status), buildOnRoot, nil)
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
			"one run with a control":    {Verdict{Status: status, Against: "job-control"}, []string{"needs 2 completed jobs", "has 1 (job-1)", "where results vary between runs", "run it again", "for a deterministic job"}},
			"its only run as control":   {Verdict{Status: status, Against: "job-1"}, []string{"has 0 (none)"}},
			"a control still running":   {Verdict{Status: status, Against: "job-control-running"}, []string{"is RUNNING"}},
			"a control of another one":  {Verdict{Status: status, Against: "job-elsewhere"}, []string{"no job of this platform experiment"}},
			"a control that is no job":  {Verdict{Status: status, Against: "job-nowhere"}, []string{"no job of this platform experiment"}},
			"a reason that is one word": {Verdict{Status: status, Despite: "obvious"}, []string{"say in a sentence", "is 7 characters"}},
		}
		// Every refusal names the way past it: the command with --despite and what it takes.
		wayOn := fmt.Sprintf("hl hypothesis status %s %s --despite", h.ID, status)
		for name, c := range refusals {
			c.says = append(c.says, wayOn, "at least 20 characters")
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
			if !errors.As((&Handler{}).hypothesisWriteError(err), &answered) || answered.GetStatus() != http.StatusConflict {
				t.Errorf("%s, %s: answered %v, want 409", status, name, (&Handler{}).hypothesisWriteError(err))
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
		if settled.StatusActor == nil || *settled.StatusActor != (domain.Actor{Kind: domain.ActorAgent, ID: "agent-1", Name: "agent-1"}) {
			t.Fatalf("who gave the verdict: got = %+v, want agent-1 by name", settled.StatusActor)
		}
	}
}

func TestAVerdictWithoutTheEvidenceStandsOnAStatedReasonWhichIsKept(t *testing.T) {
	ctx := context.Background()
	store := newPoolStore()
	svc := poolService(store)
	h, _, _, _, err := svc.RegisterHypothesis(ctx, "agent-1", agentActor("agent-1"), "pe-1", "lr 10 trains", buildOnRoot, nil)
	if err != nil {
		t.Fatal(err)
	}
	store.jobs = []*domain.Experiment{jobOn("job-1", h.ID, domain.StatusFailed)}
	// A person rules for the agent: the verdict and its basis are the agent's, and both name the person.
	store.actorNames = map[string]string{"user-ada": "Ada Lovelace"}
	settled, err := svc.SetHypothesisStatus(ctx, h.ID, "agent-1", adaActor, Verdict{Status: domain.HypothesisRefuted, Despite: enoughSaid})
	if err != nil {
		t.Fatal(err)
	}
	if len(store.comments) != 1 || !strings.Contains(store.comments[0], enoughSaid) {
		t.Fatalf("the reason was noted as %v", store.comments)
	}
	if want := (domain.Actor{Kind: domain.ActorUser, ID: "user-ada", Name: "Ada Lovelace"}); settled.AgentID != "agent-1" || settled.StatusActor == nil || *settled.StatusActor != want {
		t.Fatalf("who gave the verdict: got = %+v on %s's hypothesis, want %+v on agent-1's", settled.StatusActor, settled.AgentID, want)
	}
	if len(store.commentActors) != 1 || store.commentActors[0] != adaActor {
		t.Fatalf("who wrote the basis: got = %+v, want %+v", store.commentActors, adaActor)
	}
}

// The verdict is written to the graph and its basis to Postgres after it. When the second write
// failed, the repeat found the status already set and skipped the note, so the verdict stood
// with nothing saying what it rests on.
func TestAVerdictWhoseBasisWasNotWrittenGetsItOnTheRepeat(t *testing.T) {
	ctx := context.Background()
	store := newPoolStore()
	svc := poolService(store)
	h, _, _, _, err := svc.RegisterHypothesis(ctx, "agent-1", agentActor("agent-1"), "pe-1", "lr 10 trains", buildOnRoot, nil)
	if err != nil {
		t.Fatal(err)
	}
	store.jobs = []*domain.Experiment{jobOn("job-1", h.ID, domain.StatusFailed)}
	verdict := Verdict{Status: domain.HypothesisRefuted, Despite: enoughSaid}
	store.failNextComment = true
	if _, err := svc.SetHypothesisStatus(ctx, h.ID, "agent-1", agentActor("agent-1"), verdict); err == nil {
		t.Fatal("SetHypothesisStatus with a failing comment insert: got = nil, want an error")
	}
	if got, _ := svc.GetHypothesis(ctx, h.ID); got.Status != domain.HypothesisRefuted || len(store.comments) != 0 {
		t.Fatalf("after the failed note: got = %s with notes %v, want refuted with none", got.Status, store.comments)
	}
	var unsupported *UnsupportedVerdictError
	if _, err := svc.SetHypothesisStatus(ctx, h.ID, "agent-1", agentActor("agent-1"), Verdict{Status: domain.HypothesisRefuted}); !errors.As(err, &unsupported) {
		t.Fatalf("the repeat with no basis to note: got = %v, want it refused", err)
	}
	for range 2 {
		if _, err := svc.SetHypothesisStatus(ctx, h.ID, "agent-1", agentActor("agent-1"), verdict); err != nil {
			t.Fatalf("the repeat: got = %v, want nil", err)
		}
	}
	if len(store.comments) != 1 || !strings.Contains(store.comments[0], enoughSaid) {
		t.Fatalf("the basis after two repeats: got = %v, want it noted once", store.comments)
	}
	if _, err := svc.SetHypothesisStatus(ctx, h.ID, "agent-1", agentActor("agent-1"), Verdict{Status: domain.HypothesisRefuted}); err != nil {
		t.Errorf("saying again a verdict whose basis is noted: got = %v, want nil", err)
	}
}

// Every verdict but open needs a job of the hypothesis's own that completed or failed, whoever
// gives it and whatever reason comes with it. Open needs nothing.
func TestEveryVerdictNeedsAFinishedJobOfItsOwn(t *testing.T) {
	ctx := context.Background()
	store := newPoolStore()
	svc := poolService(store)
	h, _, _, _, err := svc.RegisterHypothesis(ctx, "agent-1", agentActor("agent-1"), "pe-1", "dropout helps", buildOnRoot, nil)
	if err != nil {
		t.Fatal(err)
	}
	store.jobs = []*domain.Experiment{jobOn("job-control", "", domain.StatusCompleted), jobOn("job-elsewhere", "other", domain.StatusCompleted),
		jobOn("job-running", h.ID, domain.StatusRunning)}
	refused := []Verdict{
		{Status: domain.HypothesisInconclusive},
		{Status: domain.HypothesisRefuted, Despite: enoughSaid},
		{Status: domain.HypothesisConfirmed, Against: "job-control", Despite: enoughSaid},
	}
	for _, verdict := range refused {
		_, err := svc.SetHypothesisStatus(ctx, h.ID, "agent-1", agentActor("agent-1"), verdict)
		var unsupported *UnsupportedVerdictError
		if !errors.As(err, &unsupported) || !strings.Contains(unsupported.Detail, string(verdict.Status)+" needs a job of this hypothesis") {
			t.Fatalf("%s with no finished job of its own: got = %v, want it refused", verdict.Status, err)
		}
	}
	if got, _ := svc.GetHypothesis(ctx, h.ID); got.Status != domain.HypothesisOpen || len(store.comments) != 0 {
		t.Fatalf("after the refusals the hypothesis is %s with notes %v", got.Status, store.comments)
	}
	if _, err := svc.SetHypothesisStatus(ctx, h.ID, "agent-1", agentActor("agent-1"), Verdict{Status: domain.HypothesisOpen}); err != nil {
		t.Fatalf("open: %v", err)
	}
	for _, finished := range []domain.ExperimentStatus{domain.StatusFailed, domain.StatusCompleted} {
		store.jobs = []*domain.Experiment{jobOn("job-1", h.ID, finished)}
		if _, err := svc.SetHypothesisStatus(ctx, h.ID, "agent-1", agentActor("agent-1"), Verdict{Status: domain.HypothesisInconclusive}); err != nil {
			t.Fatalf("inconclusive after a %s job: %v", finished, err)
		}
		if _, err := svc.SetHypothesisStatus(ctx, h.ID, "agent-1", agentActor("agent-1"), Verdict{Status: domain.HypothesisOpen}); err != nil {
			t.Fatalf("open: %v", err)
		}
	}
	if len(store.comments) != 0 {
		t.Fatalf("notes written for verdicts that claim nothing: %v", store.comments)
	}
}

// Agents in a live run wrote a reason with every verdict on a compression ratio, a value the same
// code and data give exactly on any machine. Declared deterministic, one run and the control are
// the evidence, and a run is still needed.
func TestAVerdictOnDeterministicMetricsNeedsNoRepeat(t *testing.T) {
	ctx := context.Background()
	bound := 1.0
	store := newPoolStore()
	store.metrics = []domain.MetricDefinition{
		{Key: "ratio", Direction: "maximize", Deterministic: true},
		{Key: "exact", Direction: "maximize", Role: domain.MetricRoleConstraint, Bound: &bound},
	}
	svc := poolService(store)
	h, _, _, _, err := svc.RegisterHypothesis(ctx, "agent-1", agentActor("agent-1"), "pe-1", "a joint context helps", buildOnRoot, nil)
	if err != nil {
		t.Fatal(err)
	}
	store.jobs = []*domain.Experiment{jobOn("job-control", "", domain.StatusCompleted)}
	var unsupported *UnsupportedVerdictError
	if _, err := svc.SetHypothesisStatus(ctx, h.ID, "agent-1", agentActor("agent-1"), Verdict{Status: domain.HypothesisRefuted, Against: "job-control"}); !errors.As(err, &unsupported) || !strings.Contains(unsupported.Detail, "needs a job of this hypothesis") {
		t.Fatalf("a verdict with no run of its own: got = %v, want it refused", err)
	}
	store.jobs = append(store.jobs, jobOn("job-1", h.ID, domain.StatusCompleted))
	if settled, err := svc.SetHypothesisStatus(ctx, h.ID, "agent-1", agentActor("agent-1"), Verdict{Status: domain.HypothesisRefuted, Against: "job-control"}); err != nil || settled.Status != domain.HypothesisRefuted {
		t.Fatalf("one completed run and a control on a deterministic metric: %v, %+v", err, settled)
	}
}

// Agents in a live run on a declared-deterministic compression ratio gave each verdict with a
// reason, though a job of the hypothesis and a control were on record, so each was noted as given
// without a repeated run and a control. There a control is all the job needs, and the reason is
// left for what no job of the hypothesis shows.
func TestOnDeterministicMetricsAJobAndAControlSettleTheVerdictInPlaceOfAReason(t *testing.T) {
	ctx := context.Background()
	store := newPoolStore()
	store.metrics = []domain.MetricDefinition{{Key: "ratio", Direction: "maximize", Deterministic: true}}
	svc := poolService(store)
	offline, _, _, _, err := svc.RegisterHypothesis(ctx, "agent-1", agentActor("agent-1"), "pe-1", "an entropy bound rules this out", buildOnRoot, nil)
	if err != nil {
		t.Fatal(err)
	}
	ran, _, _, _, err := svc.RegisterHypothesis(ctx, "agent-1", agentActor("agent-1"), "pe-1", "a joint context helps", buildOnRoot, nil)
	if err != nil {
		t.Fatal(err)
	}
	store.jobs = []*domain.Experiment{jobOn("job-control", "", domain.StatusCompleted), jobOn("job-control-running", "", domain.StatusRunning),
		jobOn("job-1", ran.ID, domain.StatusCompleted)}

	var unsupported *UnsupportedVerdictError
	if _, err := svc.SetHypothesisStatus(ctx, offline.ID, "agent-1", agentActor("agent-1"), Verdict{Status: domain.HypothesisRefuted, Despite: enoughSaid}); !errors.As(err, &unsupported) {
		t.Fatalf("a reason for a hypothesis no job tested: got = %v, want it refused", err)
	}
	store.jobs = append(store.jobs, jobOn("job-offline", offline.ID, domain.StatusFailed))
	if _, err := svc.SetHypothesisStatus(ctx, offline.ID, "agent-1", agentActor("agent-1"), Verdict{Status: domain.HypothesisRefuted, Despite: enoughSaid}); err != nil {
		t.Fatalf("a reason for a hypothesis whose job failed: got = %v, want it to stand", err)
	}
	if len(store.comments) != 1 || !strings.Contains(store.comments[0], "given without a repeated run and a control: "+enoughSaid) {
		t.Fatalf("the basis of a verdict with a failed job: %v", store.comments)
	}

	_, err = svc.SetHypothesisStatus(ctx, ran.ID, "agent-1", agentActor("agent-1"), Verdict{Status: domain.HypothesisRefuted, Despite: enoughSaid})
	if !errors.As(err, &unsupported) {
		t.Fatalf("a reason in place of the control for a hypothesis with a completed job: got = %v, want it refused", err)
	}
	for _, want := range []string{"declared deterministic", "job-1", "baseline_job_id", fmt.Sprintf("hl hypothesis status %s refuted --against CONTROL_JOB", ran.ID)} {
		if !strings.Contains(unsupported.Detail, want) {
			t.Errorf("refusal %q does not say %q", unsupported.Detail, want)
		}
	}
	if _, err := svc.SetHypothesisStatus(ctx, ran.ID, "agent-1", agentActor("agent-1"), Verdict{Status: domain.HypothesisRefuted, Against: "job-control-running", Despite: enoughSaid}); !errors.As(err, &unsupported) {
		t.Fatalf("a reason beside a control that is no completed job: got = %v, want it refused", err)
	}

	settled, err := svc.SetHypothesisStatus(ctx, ran.ID, "agent-1", agentActor("agent-1"), Verdict{Status: domain.HypothesisRefuted, Against: "job-control", Despite: enoughSaid})
	if err != nil || settled.Status != domain.HypothesisRefuted {
		t.Fatalf("one job and a control with a reason beside them: %v, %+v", err, settled)
	}
	if len(store.comments) != 2 || store.comments[1] != "Verdict refuted against control job-control." {
		t.Fatalf("the basis of a verdict with its control: %v", store.comments)
	}
}

// Where results vary between runs, a reason still stands in for the repeat a control alone lacks.
func TestOnVaryingMetricsAReasonStandsBesideAControlWithoutARepeat(t *testing.T) {
	ctx := context.Background()
	store := newPoolStore()
	svc := poolService(store)
	h, _, _, _, err := svc.RegisterHypothesis(ctx, "agent-1", agentActor("agent-1"), "pe-1", "a larger batch helps", buildOnRoot, nil)
	if err != nil {
		t.Fatal(err)
	}
	store.jobs = []*domain.Experiment{jobOn("job-control", "", domain.StatusCompleted), jobOn("job-1", h.ID, domain.StatusCompleted)}
	var unsupported *UnsupportedVerdictError
	if _, err := svc.SetHypothesisStatus(ctx, h.ID, "agent-1", agentActor("agent-1"), Verdict{Status: domain.HypothesisConfirmed, Against: "job-control"}); !errors.As(err, &unsupported) {
		t.Fatalf("one run and a control: got = %v, want a repeat asked for", err)
	}
	if _, err := svc.SetHypothesisStatus(ctx, h.ID, "agent-1", agentActor("agent-1"), Verdict{Status: domain.HypothesisConfirmed, Against: "job-control", Despite: enoughSaid}); err != nil {
		t.Fatalf("one run, a control and a reason: got = %v, want it to stand", err)
	}
	if len(store.comments) != 1 || !strings.Contains(store.comments[0], "given without a repeated run and a control: "+enoughSaid) {
		t.Fatalf("the basis: %v", store.comments)
	}
}
