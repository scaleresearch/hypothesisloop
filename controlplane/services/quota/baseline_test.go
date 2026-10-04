package quota

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
)

func openExperiment() *domain.PlatformExperiment {
	bound := 1.0
	return &domain.PlatformExperiment{
		ID: "pe-1", ProjectID: "prj-a", Name: "n", Description: "Train train.py and report val_loss.\nBASELINE\n  config: lr 0.1\n  measured: not yet established",
		Status: domain.PlatformExpOpen, BudgetAcceleratorHours: 6, MaxAgents: 2, ReportIntervalSeconds: 30,
		StartsAt: time.Now().UTC(), DurationHours: 4, BaselineDueAt: time.Now().UTC().Add(time.Hour), Stages: []domain.Stage{{LengthPct: 100}},
		Metrics: []domain.MetricDefinition{{Key: "val_loss", Direction: "minimize"}, {Key: "exact_match", Direction: "maximize", Role: domain.MetricRoleConstraint, Bound: &bound}},
	}
}

func measuredBaseline() *Baseline {
	domain.SetAcceleratorRates(map[string]float64{"h100": 1.0})
	return &Baseline{
		Job: &domain.Experiment{ID: "job-1", AgentID: "ag-baseline", PlatformExperimentID: "pe-1", Status: domain.StatusCompleted, CodeRef: "git://seed@abc",
			AcceleratorType: "h100", AcceleratorCount: 2, EstimatedDurationHours: 5},
		OwnerRole: domain.SignupRoleBaseline, Finding: true, Reported: map[string]float64{"val_loss": 1.8, "exact_match": 1}, ObservedHours: 0.2,
	}
}

// declareAPI is PATCH /platform-experiments/pe-1 over pe, with the platform's records saying
// baseline of whichever job is named.
func declareAPI(t *testing.T, pe *domain.PlatformExperiment, baseline *Baseline, caller auth.Principal) (*createdStore, func(body map[string]any) (int, map[string]any)) {
	t.Helper()
	store := &createdStore{created: []*domain.PlatformExperiment{pe}}
	svc := NewPlatformExperimentsService(store, testSettings, zap.NewNop(), "", time.Minute)
	svc.baseline = func(_ context.Context, _ *domain.PlatformExperiment, jobID string) (*Baseline, error) {
		if baseline == nil || baseline.Job.ID != jobID {
			return nil, nil
		}
		return baseline, nil
	}
	placement := authtest.Placement{
		Projects:            map[string]domain.Project{"prj-a": {ID: "prj-a", OrgID: "org-a"}},
		PlatformExperiments: map[string]domain.Tenancy{"pe-1": {PlatformExperimentID: "pe-1", ProjectID: "prj-a", OrgID: "org-a"}},
	}
	router := chi.NewRouter()
	doc := apidocs.New(router, "test", "1.0.0", "", authtest.As(caller), auth.NewAccess(placement), zap.NewNop())
	RegisterHuma(doc, NewHandler(nil, zap.NewNop()), NewPlatformExperimentsHandler(svc, &fakeRepositories{}, testPlanning, zap.NewNop()))
	return store, func(body map[string]any) (int, map[string]any) {
		t.Helper()
		var payload bytes.Buffer
		if err := json.NewEncoder(&payload).Encode(body); err != nil {
			t.Fatal(err)
		}
		req := authtest.Authorize(httptest.NewRequest("PATCH", "/platform-experiments/pe-1", &payload))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		out := map[string]any{}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
}

var theCoordinator = auth.Principal{Kind: auth.KindCoordinator, OrgIDs: []string{"org-a"}, ProjectID: "prj-a", PlatformExperimentID: "pe-1"}

// theMeasuredBlock is what the platform writes into the brief of an experiment whose baseline is
// measuredBaseline.
const theMeasuredBlock = "\n\n" + measuredBaselineHeader + "\njob: job-1\ncode_ref: git://seed@abc\nmeasured: val_loss = 1.8, exact_match = 1"

// A baseline was something only a draft's planner could give an experiment, inside its proposal.
// It is declared on the open experiment, by its coordinator or its owner, and the platform reads
// the job itself before it stores anything. Declaring was the job's id and a brief rewritten by
// hand to name the job and its code_ref, refused when either was missing: it is the job's id
// alone, and the platform writes what it holds of the job into the brief.
func TestABaselineIsDeclaredOnAnOpenExperimentAndCheckedAgainstTheJob(t *testing.T) {
	declaration := map[string]any{"baseline_job_id": "job-1"}
	for name, caller := range map[string]auth.Principal{"its coordinator": theCoordinator, "its owner": auth.Principal(ownerOfA)} {
		store, patch := declareAPI(t, openExperiment(), measuredBaseline(), caller)
		code, out := patch(declaration)
		stored := store.created[0]
		if code != http.StatusOK || stored.BaselineJobID != "job-1" || math.Abs(stored.JobEstimatedDurationHours-0.3) > 1e-9 || stored.Description != openExperiment().Description+theMeasuredBlock {
			t.Fatalf("%s: got = %d %v, stored baseline %q with an estimate of %g and the brief %q, want job-1, one and a half times its 0.2 hours and the brief with what the job measured", name, code, out, stored.BaselineJobID, stored.JobEstimatedDurationHours, stored.Description)
		}
		// Declared again with a brief that carries the block, the brief holds it once.
		if code, _ := patch(map[string]any{"baseline_job_id": "job-1", "description": stored.Description + "\n\nA trial runs train.py."}); code != http.StatusOK ||
			store.created[0].Description != openExperiment().Description+"\n\nA trial runs train.py."+theMeasuredBlock {
			t.Fatalf("%s: declared again: got = %d, brief %q, want the block once, after what its author wrote", name, code, store.created[0].Description)
		}
		if estimate, _ := out["job_estimated_duration_hours"].(float64); out["baseline_job_id"] != "job-1" || math.Abs(estimate-0.3) > 1e-9 {
			t.Errorf("%s: answered %v and %v, want the baseline and the estimate", name, out["baseline_job_id"], out["job_estimated_duration_hours"])
		}
	}

	for name, c := range map[string]struct {
		body   map[string]any
		change func(*Baseline, *domain.PlatformExperiment)
		reason string
	}{
		"a job that does not exist":       {map[string]any{"baseline_job_id": "job-2"}, nil, "no_baseline"},
		"no job named":                    {map[string]any{"baseline_job_id": ""}, nil, "no_baseline"},
		"a researcher's job":              {declaration, func(b *Baseline, _ *domain.PlatformExperiment) { b.OwnerRole = domain.SignupRoleCompetitor }, "no_baseline"},
		"a job of another experiment":     {declaration, func(b *Baseline, _ *domain.PlatformExperiment) { b.Job.PlatformExperimentID = "pe-other" }, "no_baseline"},
		"a job that failed":               {declaration, func(b *Baseline, _ *domain.PlatformExperiment) { b.Job.Status = domain.StatusFailed }, "baseline_not_completed"},
		"nothing filed":                   {declaration, func(b *Baseline, _ *domain.PlatformExperiment) { b.Finding = false }, "baseline_without_finding"},
		"a metric the job never reported": {declaration, func(b *Baseline, _ *domain.PlatformExperiment) { delete(b.Reported, "val_loss") }, "baseline_missing_metric"},
		"a gate the baseline fails":       {declaration, func(b *Baseline, _ *domain.PlatformExperiment) { b.Reported["exact_match"] = 0.97 }, "baseline_breaks_constraint"},
		// The baseline cost 0.4 and a trial holds 0.6, its estimate: a budget of 1 covered the
		// cost twice and neither researcher's first job.
		"a budget below one trial apiece": {declaration, func(_ *Baseline, pe *domain.PlatformExperiment) { pe.BudgetAcceleratorHours = 1 }, "budget_below_baseline"},
	} {
		pe, baseline := openExperiment(), measuredBaseline()
		if c.change != nil {
			c.change(baseline, pe)
		}
		store, patch := declareAPI(t, pe, baseline, theCoordinator)
		code, out := patch(c.body)
		if stored := store.created[0]; code != http.StatusUnprocessableEntity || out["reason"] != c.reason || stored.BaselineJobID != "" || stored.JobEstimatedDurationHours != 0 || strings.Contains(stored.Description, "job-1") {
			t.Errorf("%s: got = %d %v, stored baseline %q, want 422 %s and nothing stored", name, code, out, stored.BaselineJobID, c.reason)
		}
	}
}

// A reference is an outside system the goal compares against, which a baseline may be unable to
// run. Leaving it out does not hold the run, and the brief says it was not measured. A reference
// is measured by the baseline's follow-up jobs, and the brief lists every value a completed job
// reported and whether that job met the constraints: one that did not still shows, so nobody
// takes it for unmeasured, and says it does not count.
func TestABaselineThatLeftAReferenceOutIsDeclaredAndTheBriefSaysSo(t *testing.T) {
	pe := openExperiment()
	pe.Metrics = append(pe.Metrics,
		domain.MetricDefinition{Key: "other_loss", Direction: "minimize", Role: domain.MetricRoleReference},
		domain.MetricDefinition{Key: "third_loss", Direction: "minimize", Role: domain.MetricRoleReference})
	baseline := measuredBaseline()
	baseline.References = []MetricReference{
		{Metric: "other_loss", Values: []ReferenceValue{
			{AgentID: "ag-baseline", Value: 2.1, ExperimentID: "job-2", ConstraintsMet: metricsdb.ConstraintsHeld},
			{AgentID: "ag-baseline", Value: 1.9, ExperimentID: "job-3", ConstraintsMet: metricsdb.ConstraintsBroken},
			{AgentID: "ag-baseline", Value: 2.0, ExperimentID: "job-4", ConstraintsMet: metricsdb.ConstraintsUnknown},
		}},
		{Metric: "third_loss", Values: []ReferenceValue{}},
	}
	store, patch := declareAPI(t, pe, baseline, theCoordinator)
	want := openExperiment().Description + theMeasuredBlock +
		"\nreference other_loss = 2.1 (job job-2, constraints met)" +
		"\nreference other_loss = 1.9 (job job-3, a constraint broken, so it does not count)" +
		"\nreference other_loss = 2 (job job-4, a constraint not reported, so it does not count)" +
		"\nreferences not measured: third_loss"
	if code, out := patch(map[string]any{"baseline_job_id": "job-1"}); code != http.StatusOK || store.created[0].Description != want {
		t.Fatalf("got = %d %v, brief %q, want the baseline declared and the brief %q", code, out, store.created[0].Description, want)
	}
}

// Its metrics and its budget are what the baseline was checked against. An owner who changes one
// after the declaration is held to the same job, so no experiment starts on a baseline its
// definition has left behind. Its brief keeps what the platform wrote of the job whatever a
// later patch of it says. The run started up to a minute after the last thing it waited for: it
// starts in the request that supplies it.
func TestAnOpenExperimentIsHeldToItsDeclaredBaselineAndStartsOnceNothingIsMissing(t *testing.T) {
	const baselineBrief = "Train train.py and report val_loss." + theMeasuredBlock
	declared := func() *domain.PlatformExperiment {
		pe := openExperiment()
		pe.Description, pe.BaselineJobID, pe.JobEstimatedDurationHours = baselineBrief, "job-1", 0.3
		return pe
	}
	for name, c := range map[string]struct {
		body   map[string]any
		reason string
	}{
		"a budget one run apiece no longer fits": {map[string]any{"budget_accelerator_hours": 0.3}, "budget_below_baseline"},
		"more researchers than the budget fits":  {map[string]any{"max_agents": 40}, "budget_below_baseline"},
		"a metric the baseline never reported":   {map[string]any{"metrics": []map[string]any{{"key": "val_accuracy", "direction": "maximize"}}}, "baseline_missing_metric"},
	} {
		store, patch := declareAPI(t, declared(), measuredBaseline(), auth.Principal(ownerOfA))
		code, out := patch(c.body)
		if stored := store.created[0]; code != http.StatusUnprocessableEntity || out["reason"] != c.reason || stored.BudgetAcceleratorHours != 6 || stored.MaxAgents != 2 || len(stored.Metrics) != 2 || stored.Description != baselineBrief {
			t.Errorf("%s: got = %d %v, want 422 %s and nothing changed", name, code, out, c.reason)
		}
	}
	store, patch := declareAPI(t, declared(), measuredBaseline(), auth.Principal(ownerOfA))
	if code, out := patch(map[string]any{"budget_accelerator_hours": 8}); code != http.StatusOK || store.created[0].BudgetAcceleratorHours != 8 || store.created[0].BaselineJobID != "job-1" {
		t.Errorf("a budget the baseline fits: got = %d %v, want 200 and the baseline kept", code, out)
	}

	// max_agents counts the researchers that joined, so it is never set under them.
	// What a declared experiment still waits for is read on it.
	store, patch = declareAPI(t, declared(), measuredBaseline(), auth.Principal(ownerOfA))
	if code, out := patch(map[string]any{"budget_accelerator_hours": 8}); code != http.StatusOK || out["status_reason"].(map[string]any)["reason"] != "no_researchers" {
		t.Errorf("a declared experiment nobody joined: got = %d %v, want status_reason no_researchers", code, out["status_reason"])
	}
	if code, _ := patch(map[string]any{"description": "The brief, rewritten."}); code != http.StatusOK || store.created[0].Description != "The brief, rewritten."+theMeasuredBlock || store.started != 0 {
		t.Errorf("a brief that drops what the platform wrote: got = %d, brief %q, %d starts, want the block kept and no start", code, store.created[0].Description, store.started)
	}
	store.researchers = []string{"ag-1", "ag-2"}
	if code, out := patch(map[string]any{"max_agents": 1}); code != http.StatusConflict || out["reason"] != "max_agents_below_researchers" || store.created[0].MaxAgents != 2 {
		t.Errorf("max_agents under the researchers that joined: got = %d %v, want 409 max_agents_below_researchers and nothing changed", code, out)
	}
	if code, out := patch(map[string]any{"max_agents": 3}); code != http.StatusOK || store.created[0].MaxAgents != 3 || store.started != 1 ||
		out["status"] != "running" || out["status_reason"].(map[string]any)["reason"] != "running" {
		t.Errorf("a stored change with its baseline and its researchers: got = %d %v, %d starts, want 200 and the run started in this request", code, out, store.started)
	}

	// The declaration itself starts the run when a researcher already joined.
	store, patch = declareAPI(t, openExperiment(), measuredBaseline(), theCoordinator)
	store.researchers = []string{"ag-1"}
	if code, out := patch(map[string]any{"baseline_job_id": "job-1"}); code != http.StatusOK || store.started != 1 || out["status"] != "running" || out["started_at"] == nil {
		t.Errorf("declaring with a researcher joined: got = %d %v, %d starts, want the run started by the declaration", code, out, store.started)
	}

	// A patch states the report interval it carries: 0 was stored as it came.
	store, patch = declareAPI(t, openExperiment(), nil, auth.Principal(ownerOfA))
	if code, out := patch(map[string]any{"report_interval_seconds": 0}); code != http.StatusUnprocessableEntity || out["reason"] != "invalid_experiment" || store.created[0].ReportIntervalSeconds != 30 {
		t.Errorf("report_interval_seconds 0: got = %d %v, stored %d, want 422 invalid_experiment and 30 kept", code, out, store.created[0].ReportIntervalSeconds)
	}

	// Once it runs the baseline is what the shares were carved after, and the brief is the
	// coordinator's to keep true to what the run learns.
	running := declared()
	running.Status = domain.PlatformExpRunning
	store, patch = declareAPI(t, running, measuredBaseline(), theCoordinator)
	if code, out := patch(map[string]any{"baseline_job_id": "job-1"}); code != http.StatusConflict || out["reason"] != "experiment_running" {
		t.Errorf("declaring on a running experiment: got = %d %v, want 409 experiment_running", code, out)
	}
	if code, out := patch(map[string]any{"description": "The brief, redirected."}); code != http.StatusOK || store.created[0].BaselineJobID != "job-1" || store.created[0].JobEstimatedDurationHours != 0.3 ||
		store.created[0].Description != "The brief, redirected."+theMeasuredBlock {
		t.Errorf("a running experiment's brief: got = %d %v, brief %q, want 200 with the baseline, the estimate and what the platform wrote of the job as they were", code, out, store.created[0].Description)
	}
}
