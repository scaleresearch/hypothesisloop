package quota

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"gopkg.in/yaml.v3"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/api"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// testSettings is the platform settings a service of these tests applies and holds a create to.
var testSettings = domain.QuotaConfig{
	DefaultStages: []domain.Stage{{LengthPct: 100}}, DefaultReportIntervalSeconds: 30,
	DefaultQuotaTier: domain.QuotaTierGuaranteed, MaxRunLength: 30 * 24 * time.Hour, BaselineLimit: 90 * time.Minute, MinBaseline: 20 * time.Minute,
}

var testPlanning = Planning{Limit: 30 * time.Minute, ProposedFor: 7 * 24 * time.Hour, PlannerKept: 10 * time.Minute, AutoAccept: 30 * time.Second}

// reaching has the org of every project reach the given accelerator types and no other.
func reaching(svc *PlatformExperimentsService, types ...domain.AcceleratorType) *PlatformExperimentsService {
	reachable := map[string][]domain.Cluster{}
	for _, t := range types {
		reachable[string(t)] = []domain.Cluster{{Name: "k3s"}}
	}
	svc.reachable = func(context.Context, string) (map[string][]domain.Cluster, error) { return reachable, nil }
	return svc
}

// One check stands behind a create, a proposal and an accept, so what it refuses and what it
// takes from the settings is the same for all three.
func TestValidateRefusesWithAReasonAndAppliesTheSettings(t *testing.T) {
	valid := func() CreatePlatformExperimentRequest {
		return CreatePlatformExperimentRequest{
			Name: "exp", Description: "the brief", BudgetAcceleratorHours: 10, DurationHours: 24, MaxAgents: 5,
			Metrics: []domain.MetricDefinition{{Key: "loss", Direction: "minimize"}},
		}
	}
	svc := NewPlatformExperimentsService(&createStore{}, testSettings, zap.NewNop(), "", time.Minute)
	spec, err := svc.validate(valid())
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if spec.ReportIntervalSeconds != 30 || len(spec.Stages) != 1 || spec.DefaultQuotaTier != "guaranteed" || spec.DurationHours != 24 {
		t.Errorf("settings applied: report interval %d, stages %v, tier %q, duration %v", spec.ReportIntervalSeconds, spec.Stages, spec.DefaultQuotaTier, spec.DurationHours)
	}
	block := valid()
	block.Name, block.Description, block.DefaultQuotaTier = " exp\n", "\nthe brief\nof two lines\n", "burst_only\n"
	block.Metrics = []domain.MetricDefinition{{Key: " loss\n", Direction: "minimize\n", Role: "ranking\n"}}
	spec, err = svc.validate(block)
	if err != nil || spec.Name != "exp" || spec.Description != "the brief\nof two lines" || spec.DefaultQuotaTier != "burst_only" ||
		!reflect.DeepEqual(spec.Metrics, []domain.MetricDefinition{{Key: "loss", Direction: "minimize", Role: "ranking"}}) {
		t.Errorf("texts written as blocks: got %+v, %v, want each without the whitespace around it", spec, err)
	}
	if block.Metrics[0].Key != " loss\n" {
		t.Errorf("validate changed the metrics it was handed: %+v", block.Metrics)
	}
	twoLines := valid()
	twoLines.Metrics[0].Key = "loss\nval"
	var refused *api.Refusal
	if _, err := svc.validate(twoLines); !errors.As(err, &refused) || refused.GetStatus() != 422 || !strings.Contains(refused.Error(), "metrics[0].key is one line") {
		t.Errorf("a metric key of two lines: got %v, want 422 naming metrics[0].key", err)
	}
	for name, c := range map[string]struct {
		mutate func(*CreatePlatformExperimentRequest)
		reason string
	}{
		"no name":             {func(r *CreatePlatformExperimentRequest) { r.Name = " " }, "invalid_experiment"},
		"a name of two lines": {func(r *CreatePlatformExperimentRequest) { r.Name = "exp\nand more" }, "invalid_experiment"},
		"no description":      {func(r *CreatePlatformExperimentRequest) { r.Description = "" }, "invalid_experiment"},
		"a description too long": {func(r *CreatePlatformExperimentRequest) {
			r.Description = strings.Repeat("x", domain.MaxProseBytes+1)
		}, "invalid_experiment"},
		"no ranking metric": {func(r *CreatePlatformExperimentRequest) { r.Metrics = nil }, "invalid_metrics"},
		"negative interval": {func(r *CreatePlatformExperimentRequest) { r.ReportIntervalSeconds = -1 }, "invalid_experiment"},
		"bad stages":        {func(r *CreatePlatformExperimentRequest) { r.Stages = []domain.Stage{{LengthPct: 50}} }, "invalid_stages"},
		"bad quota tier":    {func(r *CreatePlatformExperimentRequest) { r.DefaultQuotaTier = "gold" }, "invalid_experiment"},
		"no budget":         {func(r *CreatePlatformExperimentRequest) { r.BudgetAcceleratorHours = 0 }, "invalid_cost"},
		"no max_agents":     {func(r *CreatePlatformExperimentRequest) { r.MaxAgents = 0 }, "invalid_agents"},
		"no length":         {func(r *CreatePlatformExperimentRequest) { r.DurationHours = 0 }, "invalid_length"},
		"longer than a run": {func(r *CreatePlatformExperimentRequest) { r.DurationHours = 31 * 24 }, "run_too_long"},
		"a negative length": {func(r *CreatePlatformExperimentRequest) { r.DurationHours = -1 }, "invalid_length"},
	} {
		req := valid()
		c.mutate(&req)
		_, err := svc.validate(req)
		var refused *api.Refusal
		if !errors.As(err, &refused) || refused.GetStatus() != 422 || refused.Reason != c.reason {
			t.Errorf("%s: got %v, want 422 %s", name, err, c.reason)
		}
	}
}

// The ladder an experiment is stored with is the one its run starts with: a field no boundary can
// cut from takes the default as one stage, and a ladder stated for it is refused.
func TestAFieldNoBoundaryCanCutIsStoredWithTheLadderItRuns(t *testing.T) {
	ladder := []domain.Stage{{LengthPct: 40, EvictPct: 75, MaxJobHours: 2}, {LengthPct: 60, MaxJobHours: 6}}
	settings := testSettings
	settings.DefaultStages = ladder
	svc := NewPlatformExperimentsService(&createStore{}, settings, zap.NewNop(), "", time.Minute)
	spec := func(researchers int, stages []domain.Stage) CreatePlatformExperimentRequest {
		return CreatePlatformExperimentRequest{
			Name: "exp", Description: "the brief", BudgetAcceleratorHours: 10, DurationHours: 24, MaxAgents: researchers,
			Metrics: []domain.MetricDefinition{{Key: "loss", Direction: "minimize"}}, Stages: stages,
		}
	}
	for _, researchers := range []int{2, domain.MinResearchersForCut} {
		checked, err := svc.validate(spec(researchers, nil))
		if err != nil {
			t.Fatalf("%d researchers, stages left out: %v", researchers, err)
		}
		if want := domain.RunStages(ladder, researchers); !reflect.DeepEqual(checked.Stages, want) {
			t.Errorf("%d researchers, stages left out: stored %+v, want the ladder the run starts with %+v", researchers, checked.Stages, want)
		}
	}
	var refused *api.Refusal
	if _, err := svc.validate(spec(2, ladder)); !errors.As(err, &refused) || refused.GetStatus() != 422 || refused.Reason != "invalid_stages" {
		t.Errorf("two stages stated for 2 researchers: got %v, want 422 invalid_stages", err)
	}
	if _, err := svc.validate(spec(domain.MinResearchersForCut, ladder)); err != nil {
		t.Errorf("two stages stated for %d researchers: %v", domain.MinResearchersForCut, err)
	}
}

// A platform experiment comes into being only where something could run it: a create in an org
// whose jobs a cluster takes now, a proposal and an accept sized for a type one of them has.
func TestAPlatformExperimentNoClusterCouldRunIsRefusedWhereItComesIntoBeing(t *testing.T) {
	rtx, a100 := domain.AcceleratorType("rtx-4090"), domain.AcceleratorType("a100")
	for name, c := range map[string]struct {
		reachable []domain.AcceleratorType
		sizedFor  *domain.AcceleratorType
		refused   bool
	}{
		"no type stated, a cluster reports":      {[]domain.AcceleratorType{domain.CPUType}, nil, false},
		"no type stated, no cluster reports":     {nil, nil, true},
		"a type a cluster has":                   {[]domain.AcceleratorType{domain.CPUType, rtx}, &rtx, false},
		"a type no cluster has":                  {[]domain.AcceleratorType{domain.CPUType, rtx}, &a100, true},
		"a type stated while no cluster reports": {nil, &rtx, true},
	} {
		svc := reaching(NewPlatformExperimentsService(&createStore{}, testSettings, zap.NewNop(), "", time.Minute), c.reachable...)
		_, err := svc.requireReachable(context.Background(), "prj-a", c.sizedFor)
		var refused *api.Refusal
		if got := errors.As(err, &refused) && refused.GetStatus() == 422 && refused.Reason == "accelerator_unreachable"; got != c.refused || (!c.refused && err != nil) {
			t.Errorf("%s: got %v, refused wanted: %v", name, err, c.refused)
		}
	}

	body := CreatePlatformExperimentRequest{
		Name: "exp", Description: "the brief", BudgetAcceleratorHours: 10, DurationHours: 24, MaxAgents: 5,
		Metrics: []domain.MetricDefinition{{Key: "loss", Direction: "minimize"}},
	}
	store := &createStore{}
	_, err := reaching(NewPlatformExperimentsService(store, testSettings, zap.NewNop(), "", time.Minute)).Create(context.Background(), body)
	var refused *api.Refusal
	if !errors.As(err, &refused) || refused.Reason != "accelerator_unreachable" || store.created != nil {
		t.Errorf("a create no cluster could run: got %v, stored %v, want 422 accelerator_unreachable and nothing stored", err, store.created)
	}
	unread := NewPlatformExperimentsService(&createStore{}, testSettings, zap.NewNop(), "", time.Minute)
	if _, err := unread.Create(context.Background(), body); !errors.Is(err, ErrLiveCapacityUnconfigured) {
		t.Errorf("a create with no capacity to read: got %v, want ErrLiveCapacityUnconfigured", err)
	}
}

// One that exists is changed whatever its org's clusters report: a patch reads no capacity.
func TestAPatchPassesWithNoCapacityReported(t *testing.T) {
	for _, status := range []domain.PlatformExperimentStatus{domain.PlatformExpOpen, domain.PlatformExpRunning} {
		store := &createdStore{created: []*domain.PlatformExperiment{{
			ID: "pe-1", ProjectID: "prj-a", Name: "exp", Description: "the brief", Status: status,
			BudgetAcceleratorHours: 5, MaxAgents: 3, ReportIntervalSeconds: 10,
			StartsAt: time.Now().UTC(), DurationHours: 24,
			Metrics: []domain.MetricDefinition{{Key: "loss", Direction: "minimize"}},
		}}}
		svc := reaching(NewPlatformExperimentsService(store, testSettings, zap.NewNop(), "", time.Minute))
		renamed := "renamed"
		pe, err := svc.Update(context.Background(), "pe-1", PlatformExperimentPatch{Name: &renamed})
		if err != nil || pe.Name != renamed {
			t.Errorf("%s, a rename with no capacity reported: got %v, %v", status, pe, err)
		}
	}
}

// The example is what an operator copies, so the server must accept it as written.
func TestTheExamplePlatformExperimentIsAccepted(t *testing.T) {
	raw, err := os.ReadFile("../../settings/examples/experiment.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := yaml.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	var req CreatePlatformExperimentRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	svc := reaching(NewPlatformExperimentsService(&createStore{}, testSettings, zap.NewNop(), "", time.Minute), domain.CPUType)
	if _, err := svc.Create(context.Background(), req); err != nil {
		t.Fatalf("Create from the example: %v", err)
	}
}

// A run that continues another ranks what that one ranked: its metric keys and their directions,
// in any order, with constraint bounds free to move.
func TestARunThatContinuesAnotherKeepsItsMetric(t *testing.T) {
	bound, looser := 2.0, 3.0
	prior := &domain.PlatformExperiment{ID: "pe-prior", Metrics: []domain.MetricDefinition{
		{Key: "val_loss", Direction: "minimize"}, {Key: "latency_ms", Direction: "minimize", Role: domain.MetricRoleConstraint, Bound: &bound},
	}}
	svc := NewPlatformExperimentsService(&createStore{created: prior}, testSettings, zap.NewNop(), "", time.Minute)
	if err := svc.requireContinuedMetrics(context.Background(), "pe-prior", []domain.MetricDefinition{
		{Key: "latency_ms", Direction: "minimize", Role: domain.MetricRoleConstraint, Bound: &looser}, {Key: "val_loss", Direction: "minimize"},
	}); err != nil {
		t.Errorf("the same keys and directions: %v", err)
	}
	if err := svc.requireContinuedMetrics(context.Background(), "", []domain.MetricDefinition{{Key: "accuracy", Direction: "maximize"}}); err != nil {
		t.Errorf("a fresh run declares any metric: %v", err)
	}
	for name, metrics := range map[string][]domain.MetricDefinition{
		"another key":       {{Key: "val_loss", Direction: "minimize"}, {Key: "throughput", Direction: "maximize"}},
		"another direction": {{Key: "val_loss", Direction: "maximize"}, {Key: "latency_ms", Direction: "minimize"}},
		"a metric left out": {{Key: "val_loss", Direction: "minimize"}},
	} {
		err := svc.requireContinuedMetrics(context.Background(), "pe-prior", metrics)
		var refused *api.Refusal
		if !errors.As(err, &refused) || refused.GetStatus() != 422 || refused.Reason != "invalid_metrics" ||
			!strings.Contains(refused.Error(), "latency_ms (minimize), val_loss (minimize)") {
			t.Errorf("%s: got %v, want 422 invalid_metrics naming both runs' metrics", name, err)
		}
	}
}

// continuedStore is a run to continue whose baseline job ran a commit of the repository.
type continuedStore struct {
	createStore
	baseline *domain.Experiment
}

func (f *continuedStore) GetExperiment(context.Context, string) (*domain.Experiment, error) {
	return f.baseline, nil
}

// Researchers start from start_commit, resolved to its full sha, only where it builds on the
// commit the baseline starts from: that of the baseline job of the run continued.
func TestAStartCommitDescendsFromTheBaselineOfTheRunContinued(t *testing.T) {
	const control, best, elsewhere = "b652eb57aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "a2cac09bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "cccccccccccccccccccccccccccccccccccccccc"
	store := &continuedStore{
		createStore: createStore{created: &domain.PlatformExperiment{ID: "pe-prior", BaselineJobID: "job-control"}},
		baseline:    &domain.Experiment{ID: "job-control", CodeRef: "https://github.com/Acme/Model.git@" + control},
	}
	svc := NewPlatformExperimentsService(store, testSettings, zap.NewNop(), "", time.Minute).
		WithRepositories(&fakeRepositories{commits: map[string][]string{control: nil, best: {control}, elsewhere: nil}})
	ctx := context.Background()
	for start, want := range map[string]string{"": "", "a2cac09": best, "A2CAC09": best, control: control} {
		if got, err := svc.requireStart(ctx, "prj", testRepository, "pe-prior", start); err != nil || got != want {
			t.Errorf("start_commit %q = %q, %v, want %q", start, got, err, want)
		}
	}
	for name, tc := range map[string]struct{ continues, start, reason string }{
		"no sha":          {"pe-prior", "a2cac0", "invalid_start_commit"},
		"not hex":         {"pe-prior", "a2cac0z", "invalid_start_commit"},
		"a fresh run":     {"", "a2cac09", "invalid_start_commit"},
		"not in the repo": {"pe-prior", "deadbeef", "invalid_start_commit"},
		"another lineage": {"pe-prior", "ccccccc", "start_commit_not_descendant"},
	} {
		_, err := svc.requireStart(ctx, "prj", testRepository, tc.continues, tc.start)
		var refused *api.Refusal
		if !errors.As(err, &refused) || refused.GetStatus() != 422 || refused.Reason != tc.reason {
			t.Errorf("%s: got %v, want 422 %s", name, err, tc.reason)
		}
	}
}

// A run states which earlier runs its agents read, prior_runs where it states none.
func TestHistoryIsPriorRunsOrNone(t *testing.T) {
	for stated, want := range map[string]domain.History{"": domain.HistoryPriorRuns, "prior_runs": domain.HistoryPriorRuns, "none": domain.HistoryNone} {
		if got, err := resolveHistory(stated); err != nil || got != want {
			t.Errorf("history %q = %q, %v, want %q", stated, got, err, want)
		}
	}
	var refused *api.Refusal
	if _, err := resolveHistory("own"); !errors.As(err, &refused) || refused.Reason != "invalid_history" {
		t.Errorf("history own: got %v, want 422 invalid_history", err)
	}
}
