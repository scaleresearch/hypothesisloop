package quota

import (
	"context"
	"encoding/json"
	"errors"
	"os"
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
	DefaultQuotaTier: domain.QuotaTierGuaranteed, MaxRunLength: 30 * 24 * time.Hour, BaselineLimit: 90 * time.Minute,
}

var testPlanning = Planning{Limit: 30 * time.Minute, ProposedFor: 7 * 24 * time.Hour, MaxConcurrentPerOrg: 2, MaxPerDayPerOrg: 10}

// reaching has the org of every project reach the given accelerator types and no other.
func reaching(svc *PlatformExperimentsService, types ...domain.AcceleratorType) *PlatformExperimentsService {
	reachable := map[string]bool{}
	for _, t := range types {
		reachable[string(t)] = true
	}
	svc.reachable = func(context.Context, string) (map[string]bool, error) { return reachable, nil }
	return svc
}

// One check stands behind a create, a proposal and an accept, so what it refuses and what it
// takes from the settings is the same for all three.
func TestValidateRefusesWithAReasonAndAppliesTheSettings(t *testing.T) {
	now := time.Now().UTC()
	valid := func() CreatePlatformExperimentRequest {
		return CreatePlatformExperimentRequest{
			Name: "exp", Description: "the brief", BudgetAcceleratorHours: 10, DurationHours: 24, MaxAgents: 5,
			Metrics: []domain.MetricDefinition{{Key: "loss", Direction: "minimize"}},
		}
	}
	svc := reaching(NewPlatformExperimentsService(&createStore{}, testSettings, zap.NewNop(), "", time.Minute), domain.CPUType)
	spec, err := svc.validate(context.Background(), valid(), now)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if spec.ReportIntervalSeconds != 30 || len(spec.Stages) != 1 || spec.DefaultQuotaTier != "guaranteed" || !spec.EndsAt.Equal(now.Add(24*time.Hour)) {
		t.Errorf("settings applied: report interval %d, stages %v, tier %q, ends_at %v", spec.ReportIntervalSeconds, spec.Stages, spec.DefaultQuotaTier, spec.EndsAt)
	}
	for name, c := range map[string]struct {
		mutate func(*CreatePlatformExperimentRequest)
		reason string
	}{
		"no name":           {func(r *CreatePlatformExperimentRequest) { r.Name = " " }, "invalid_experiment"},
		"no description":    {func(r *CreatePlatformExperimentRequest) { r.Description = "" }, "invalid_experiment"},
		"no ranking metric": {func(r *CreatePlatformExperimentRequest) { r.Metrics = nil }, "invalid_metrics"},
		"negative interval": {func(r *CreatePlatformExperimentRequest) { r.ReportIntervalSeconds = -1 }, "invalid_experiment"},
		"bad stages":        {func(r *CreatePlatformExperimentRequest) { r.Stages = []domain.Stage{{LengthPct: 50}} }, "invalid_stages"},
		"bad quota tier":    {func(r *CreatePlatformExperimentRequest) { r.DefaultQuotaTier = "gold" }, "invalid_experiment"},
		"no budget":         {func(r *CreatePlatformExperimentRequest) { r.BudgetAcceleratorHours = 0 }, "invalid_cost"},
		"no max_agents":     {func(r *CreatePlatformExperimentRequest) { r.MaxAgents = 0 }, "invalid_agents"},
		"no length":         {func(r *CreatePlatformExperimentRequest) { r.DurationHours = 0 }, "invalid_length"},
		"longer than a run": {func(r *CreatePlatformExperimentRequest) { r.DurationHours = 31 * 24 }, "run_too_long"},
		"an end decades out": {func(r *CreatePlatformExperimentRequest) {
			r.DurationHours, r.EndsAt = 0, time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
		}, "run_too_long"},
	} {
		req := valid()
		c.mutate(&req)
		_, err := svc.validate(context.Background(), req, now)
		var refused *api.Refusal
		if !errors.As(err, &refused) || refused.GetStatus() != 422 || refused.Reason != c.reason {
			t.Errorf("%s: got %v, want 422 %s", name, err, c.reason)
		}
	}
}

// A platform experiment nothing could run is refused whoever states it: a create or a patch in
// an org whose jobs no cluster takes now, and a proposal sized for a type none of them has.
func TestValidateRefusesWhatNoClusterCouldRun(t *testing.T) {
	now := time.Now().UTC()
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
		_, err := svc.validate(context.Background(), CreatePlatformExperimentRequest{
			Name: "exp", Description: "the brief", BudgetAcceleratorHours: 10, DurationHours: 24, MaxAgents: 5,
			Metrics: []domain.MetricDefinition{{Key: "loss", Direction: "minimize"}}, acceleratorType: c.sizedFor,
		}, now)
		var refused *api.Refusal
		if got := errors.As(err, &refused) && refused.GetStatus() == 422 && refused.Reason == "accelerator_unreachable"; got != c.refused || (!c.refused && err != nil) {
			t.Errorf("%s: got %v, refused wanted: %v", name, err, c.refused)
		}
	}

	unread := NewPlatformExperimentsService(&createStore{}, testSettings, zap.NewNop(), "", time.Minute)
	if _, err := unread.Create(context.Background(), CreatePlatformExperimentRequest{
		Name: "exp", Description: "the brief", BudgetAcceleratorHours: 10, DurationHours: 24, MaxAgents: 5,
		Metrics: []domain.MetricDefinition{{Key: "loss", Direction: "minimize"}},
	}); !errors.Is(err, ErrLiveCapacityUnconfigured) {
		t.Errorf("a create with no capacity to read: got %v, want ErrLiveCapacityUnconfigured", err)
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
