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

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

func TestCreatePlatformExperimentMarksRequestErrorsInvalid(t *testing.T) {
	valid := func() CreatePlatformExperimentRequest {
		return CreatePlatformExperimentRequest{
			Name: "exp", BudgetAcceleratorHours: 10, StartsAt: time.Now(), EndsAt: time.Now().Add(time.Hour),
			MaxAgents: 5, ReportIntervalSeconds: 10,
			Metrics: []domain.MetricDefinition{{Key: "loss", Direction: "minimize"}},
			Stages:  []domain.Stage{{LengthPct: 100, EvictPct: 0}},
		}
	}
	cases := map[string]func(*CreatePlatformExperimentRequest){
		"missing max_agents":      func(r *CreatePlatformExperimentRequest) { r.MaxAgents = 0 },
		"missing report interval": func(r *CreatePlatformExperimentRequest) { r.ReportIntervalSeconds = 0 },
		"no ranking metric":       func(r *CreatePlatformExperimentRequest) { r.Metrics = nil },
		"bad stages":              func(r *CreatePlatformExperimentRequest) { r.Stages = []domain.Stage{{LengthPct: 50}} },
		"bad quota tier":          func(r *CreatePlatformExperimentRequest) { r.DefaultQuotaTier = "gold" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			svc := newSignupTestService(t, &createStore{})
			req := valid()
			mutate(&req)
			_, err := svc.Create(context.Background(), req)
			if !errors.Is(err, ErrInvalidPlatformExperiment) {
				t.Fatalf("Create: got %v, want ErrInvalidPlatformExperiment", err)
			}
		})
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
	svc := NewPlatformExperimentsService(&createStore{}, domain.QuotaConfig{DefaultStages: []domain.Stage{{LengthPct: 100}}}, zap.NewNop(), "", time.Minute)
	if _, err := svc.Create(context.Background(), req); err != nil {
		t.Fatalf("Create from the example: %v", err)
	}
}
