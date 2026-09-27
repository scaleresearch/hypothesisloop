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
)

const baselineBrief = "Train train.py and report val_loss.\nBASELINE\n  config: lr 0.1\n  code_ref: git://seed@abc\n  metric: val_loss = 1.8\n  measured: job-1"

func openExperiment() *domain.PlatformExperiment {
	bound := 1.0
	return &domain.PlatformExperiment{
		ID: "pe-1", ProjectID: "prj-a", Name: "n", Description: "Train train.py and report val_loss.\nBASELINE\n  config: lr 0.1\n  measured: not yet established",
		Status: domain.PlatformExpOpen, BudgetAcceleratorHours: 6, MaxAgents: 2, ReportIntervalSeconds: 30,
		StartsAt: time.Now().UTC(), EndsAt: time.Now().UTC().Add(4 * time.Hour), Stages: []domain.Stage{{LengthPct: 100}},
		Metrics: []domain.MetricDefinition{{Key: "val_loss", Direction: "minimize"}, {Key: "exact_match", Direction: "maximize", Role: domain.MetricRoleConstraint, Bound: &bound}},
	}
}

func measuredBaseline() *Baseline {
	return &Baseline{
		Job:       &domain.Experiment{ID: "job-1", AgentID: "ag-baseline", PlatformExperimentID: "pe-1", Status: domain.StatusCompleted, CodeRef: "git://seed@abc"},
		OwnerRole: domain.SignupRoleBaseline, Finding: true, Reported: map[string]float64{"val_loss": 1.8, "exact_match": 1}, AcceleratorHours: 0.2, ObservedHours: 0.2,
	}
}

// declareAPI is PATCH /platform-experiments/pe-1 over pe, with the platform's records saying
// baseline of whichever job is named.
func declareAPI(t *testing.T, pe *domain.PlatformExperiment, baseline *Baseline, caller auth.Principal) (*createdStore, func(body map[string]any) (int, map[string]any)) {
	t.Helper()
	store := &createdStore{created: []*domain.PlatformExperiment{pe}}
	svc := reaching(NewPlatformExperimentsService(store, testSettings, zap.NewNop(), "", time.Minute), domain.CPUType)
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

// A baseline was something only a draft's planner could give an experiment, inside its proposal.
// It is declared on the open experiment, by its coordinator or its owner, with the brief that
// hands it over, and the platform reads the job itself before it stores anything.
func TestABaselineIsDeclaredOnAnOpenExperimentAndCheckedAgainstTheJob(t *testing.T) {
	declaration := map[string]any{"description": baselineBrief, "baseline_job_id": "job-1"}
	for name, caller := range map[string]auth.Principal{"its coordinator": theCoordinator, "its owner": auth.Principal(ownerOfA)} {
		store, patch := declareAPI(t, openExperiment(), measuredBaseline(), caller)
		code, out := patch(declaration)
		stored := store.created[0]
		if code != http.StatusOK || stored.BaselineJobID != "job-1" || math.Abs(stored.JobEstimatedDurationHours-0.3) > 1e-9 || stored.Description != baselineBrief {
			t.Fatalf("%s: got = %d %v, stored baseline %q with an estimate of %g, want job-1 and one and a half times its 0.2 hours", name, code, out, stored.BaselineJobID, stored.JobEstimatedDurationHours)
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
		"a job that does not exist":       {map[string]any{"description": baselineBrief, "baseline_job_id": "job-2"}, nil, "no_baseline"},
		"no job named":                    {map[string]any{"description": baselineBrief, "baseline_job_id": ""}, nil, "no_baseline"},
		"a researcher's job":              {declaration, func(b *Baseline, _ *domain.PlatformExperiment) { b.OwnerRole = domain.SignupRoleCompetitor }, "no_baseline"},
		"a job of another experiment":     {declaration, func(b *Baseline, _ *domain.PlatformExperiment) { b.Job.PlatformExperimentID = "pe-other" }, "no_baseline"},
		"a job that failed":               {declaration, func(b *Baseline, _ *domain.PlatformExperiment) { b.Job.Status = domain.StatusFailed }, "baseline_not_completed"},
		"nothing filed":                   {declaration, func(b *Baseline, _ *domain.PlatformExperiment) { b.Finding = false }, "baseline_without_finding"},
		"a brief left as it was":          {map[string]any{"baseline_job_id": "job-1"}, nil, "brief_without_baseline"},
		"a brief without the commit":      {map[string]any{"description": "BASELINE measured: job-1", "baseline_job_id": "job-1"}, nil, "brief_without_baseline"},
		"a metric the job never reported": {declaration, func(b *Baseline, _ *domain.PlatformExperiment) { delete(b.Reported, "val_loss") }, "baseline_missing_metric"},
		"a gate the baseline fails":       {declaration, func(b *Baseline, _ *domain.PlatformExperiment) { b.Reported["exact_match"] = 0.97 }, "baseline_breaks_constraint"},
		"a budget below one run apiece":   {declaration, func(_ *Baseline, pe *domain.PlatformExperiment) { pe.BudgetAcceleratorHours = 0.3 }, "budget_below_baseline"},
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

// Its metrics, its budget and its brief are what the baseline was checked against. An owner who
// changes one after the declaration is held to the same job, so no experiment starts on a
// baseline its definition has left behind.
func TestAnOpenExperimentIsHeldToItsDeclaredBaselineByEveryLaterChange(t *testing.T) {
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
		"a brief that drops the baseline":        {map[string]any{"description": "Train train.py."}, "brief_without_baseline"},
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

	// Once it runs the baseline is what the shares were carved after, and the brief is the
	// coordinator's to keep true to what the run learns.
	running := declared()
	running.Status = domain.PlatformExpRunning
	store, patch = declareAPI(t, running, measuredBaseline(), theCoordinator)
	if code, out := patch(map[string]any{"baseline_job_id": "job-1"}); code != http.StatusConflict || out["reason"] != "experiment_running" {
		t.Errorf("declaring on a running experiment: got = %d %v, want 409 experiment_running", code, out)
	}
	if code, out := patch(map[string]any{"description": "The brief, redirected."}); code != http.StatusOK || store.created[0].BaselineJobID != "job-1" || store.created[0].JobEstimatedDurationHours != 0.3 {
		t.Errorf("a running experiment's brief: got = %d %v, want 200 with the baseline and the estimate as they were", code, out)
	}
}
