package quota

import (
	"net/http"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// runningOnBaseline is pe-1 running on the baseline measuredBaseline declared.
func runningOnBaseline() *domain.PlatformExperiment {
	pe := openExperiment()
	pe.Status = domain.PlatformExpRunning
	pe.Description, pe.BaselineJobID, pe.JobEstimatedDurationHours = "Train train.py and report val_loss."+theMeasuredBlock, "job-1", 0.3
	return pe
}

// strongerControl is a researcher's job, on its own branch, that ran the baseline's commit with
// one of its settings switched and reached a better val_loss than the declared baseline.
func strongerControl() *Baseline {
	b := measuredBaseline()
	b.Job.ID, b.Job.AgentID, b.Job.Branch = "job-2", "ag-r1", "hl/pe-1/researcher-1/coder"
	b.Job.Job.Env = map[string]string{"CODER": "rans"}
	b.OwnerRole, b.Reported = domain.SignupRoleCompetitor, map[string]float64{"val_loss": 1.6, "exact_match": 1}
	return b
}

const raiseReason = "job-2 is the control with its other coder, which the control already has"

// A baseline agent declared the control at its default configuration, and a researcher's job that only
// switched a setting the control already had beat it: every researcher beat the baseline by that
// switch, and the ranking measured nothing. While the run goes on, its coordinator or its owner
// raises the baseline to the job that measured the stronger configuration, whoever ran it, with
// the reason; the brief's measured block, the history and every agent follow.
func TestARunningExperimentsBaselineIsRaisedToAStrongerJobOfTheControl(t *testing.T) {
	raise := map[string]any{"baseline_job_id": "job-2", "baseline_reason": raiseReason}
	for name, c := range map[string]struct {
		caller auth.Principal
		kind   domain.ActorKind
		who    string
	}{
		"its coordinator": {theCoordinator, domain.ActorCoordinator, "its coordinator"},
		"its owner":       {auth.Principal(ownerOfA), domain.ActorUser, "its owner"},
	} {
		store, patch := jobsAPI(t, runningOnBaseline(), []*Baseline{measuredBaseline(), strongerControl()}, c.caller)
		code, out := patch(raise)
		stored := store.created[0]
		wantBrief := "Train train.py and report val_loss.\n\n" + measuredBaselineHeader + "\njob: job-2\ncode_ref: git://seed@abc\nenv: CODER=rans\nmeasured: val_loss = 1.6, exact_match = 1"
		if code != http.StatusOK || stored.BaselineJobID != "job-2" || stored.Description != wantBrief || stored.JobEstimatedDurationHours != 0.3 || stored.Status != domain.PlatformExpRunning {
			t.Fatalf("%s: got = %d %v, stored %q, brief %q, estimate %g, want job-2 with its block and the estimate kept", name, code, out, stored.BaselineJobID, stored.Description, stored.JobEstimatedDurationHours)
		}
		if len(stored.BaselineRaises) != 1 {
			t.Fatalf("%s: raises = %+v, want one", name, stored.BaselineRaises)
		}
		r := stored.BaselineRaises[0]
		if r.From != "job-1" || r.To != "job-2" || r.Reason != raiseReason || r.Actor.Kind != c.kind ||
			!strings.Contains(r.Detail, "raised to val_loss = 1.6 by job job-2 (from 1.8, job job-1), by "+c.who+": "+raiseReason) || !strings.Contains(r.Detail, "Re-read the brief") {
			t.Errorf("%s: raise = %+v, want from job-1 to job-2 by %s with the values and the reason", name, r, c.kind)
		}
		if raises, _ := out["baseline_raises"].([]any); out["baseline_job_id"] != "job-2" || len(raises) != 1 {
			t.Errorf("%s: answered %v with raises %v, want job-2 and the raise", name, out["baseline_job_id"], out["baseline_raises"])
		}
	}

	// A brief rewritten in the same request keeps what its author wrote, with the new block.
	store, patch := jobsAPI(t, runningOnBaseline(), []*Baseline{measuredBaseline(), strongerControl()}, theCoordinator)
	if code, out := patch(map[string]any{"baseline_job_id": "job-2", "baseline_reason": raiseReason, "description": "Start from the rANS coder."}); code != http.StatusOK ||
		!strings.HasPrefix(store.created[0].Description, "Start from the rANS coder.\n\n"+measuredBaselineHeader+"\njob: job-2") {
		t.Errorf("raised with a new brief: got = %d %v, brief %q", code, out, store.created[0].Description)
	}

	for name, c := range map[string]struct {
		body   map[string]any
		change func(current, candidate *Baseline)
		status int
		reason string
	}{
		"no reason":                      {map[string]any{"baseline_job_id": "job-2"}, nil, 422, "baseline_reason_required"},
		"an empty reason":                {map[string]any{"baseline_job_id": "job-2", "baseline_reason": "  "}, nil, 422, "baseline_reason_required"},
		"a reason and no job":            {map[string]any{"baseline_reason": raiseReason}, nil, 422, "baseline_job_required"},
		"the baseline itself":            {map[string]any{"baseline_job_id": "job-1", "baseline_reason": raiseReason}, nil, 422, "baseline_unchanged"},
		"a weaker job":                   {raise, func(_, b *Baseline) { b.Reported["val_loss"] = 1.9 }, 422, "baseline_not_stronger"},
		"a job only as good":             {raise, func(_, b *Baseline) { b.Reported["val_loss"] = 1.8 }, 422, "baseline_not_stronger"},
		"a researcher's own code":        {raise, func(_, b *Baseline) { b.Job.CodeRef = "git://seed@def" }, 422, "baseline_code_differs"},
		"a job that does not exist":      {map[string]any{"baseline_job_id": "job-9", "baseline_reason": raiseReason}, nil, 422, "no_baseline"},
		"a job of another experiment":    {raise, func(_, b *Baseline) { b.Job.PlatformExperimentID = "pe-other" }, 422, "no_baseline"},
		"a job that failed":              {raise, func(_, b *Baseline) { b.Job.Status = domain.StatusFailed }, 422, "baseline_not_completed"},
		"nothing filed":                  {raise, func(_, b *Baseline) { b.Finding = false }, 422, "baseline_without_finding"},
		"a gate it fails":                {raise, func(_, b *Baseline) { b.Reported["exact_match"] = 0.9 }, 422, "baseline_breaks_constraint"},
		"a metric it never reported":     {raise, func(_, b *Baseline) { delete(b.Reported, "exact_match") }, 422, "baseline_missing_metric"},
		"another environment than ran":   {raise, func(_, b *Baseline) { b.Declared = &[]string{branchEnvironment}[0] }, 422, "baseline_environment_differs"},
		"another field of a running one": {map[string]any{"baseline_job_id": "job-2", "baseline_reason": raiseReason, "budget_accelerator_hours": 9}, nil, 409, "experiment_running"},
	} {
		current, candidate := measuredBaseline(), strongerControl()
		if c.change != nil {
			c.change(current, candidate)
		}
		caller := theCoordinator
		if c.status == 409 {
			caller = auth.Principal(ownerOfA)
		}
		store, patch := jobsAPI(t, runningOnBaseline(), []*Baseline{current, candidate}, caller)
		code, out := patch(c.body)
		if stored := store.created[0]; code != c.status || out["reason"] != c.reason || stored.BaselineJobID != "job-1" || len(stored.BaselineRaises) != 0 || strings.Contains(stored.Description, "job-2") {
			t.Errorf("%s: got = %d %v, stored %q, want %d %s and nothing changed", name, code, out, stored.BaselineJobID, c.status, c.reason)
		}
	}

	// A job of another branch whose commit declares the same environment file ran in it.
	candidate := strongerControl()
	ranIn(candidate, branchEnvironment)
	candidate.Declared = &[]string{branchEnvironment}[0]
	store, patch = jobsAPI(t, runningOnBaseline(), []*Baseline{measuredBaseline(), candidate}, theCoordinator)
	if code, out := patch(raise); code != http.StatusOK || store.created[0].BaselineJobID != "job-2" {
		t.Errorf("a job of another branch in the baseline's environment: got = %d %v, want raised", code, out)
	}
}

// While it is open the baseline is its baseline agent's to measure and the job it finishes with
// is declared; once it is closed nothing changes. A raise is refused in both.
func TestTheBaselineIsRaisedOnlyWhileTheRunGoesOn(t *testing.T) {
	raise := map[string]any{"baseline_job_id": "job-2", "baseline_reason": raiseReason}
	store, patch := jobsAPI(t, openExperiment(), []*Baseline{measuredBaseline(), strongerControl()}, theCoordinator)
	if code, out := patch(raise); code != http.StatusConflict || out["reason"] != "baseline_not_running" || store.created[0].BaselineJobID != "" {
		t.Errorf("open: got = %d %v, want 409 baseline_not_running and nothing declared", code, out)
	}
	closed := runningOnBaseline()
	closed.Status = domain.PlatformExpClosed
	store, patch = jobsAPI(t, closed, []*Baseline{measuredBaseline(), strongerControl()}, theCoordinator)
	if code, out := patch(raise); code != http.StatusConflict || out["reason"] != "experiment_not_editable" || store.created[0].BaselineJobID != "job-1" {
		t.Errorf("closed: got = %d %v, want 409 experiment_not_editable and the baseline kept", code, out)
	}
}
