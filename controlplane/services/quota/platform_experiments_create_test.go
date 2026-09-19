package quota

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

func TestCreatePlatformExperimentMarksRequestErrorsInvalid(t *testing.T) {
	valid := func() CreatePlatformExperimentRequest {
		return CreatePlatformExperimentRequest{
			Name: "exp", BudgetAcceleratorHours: 10, StartsAt: time.Now(), EndsAt: time.Now().Add(time.Hour),
			Metrics: []domain.MetricDefinition{{Key: "loss", Direction: "minimize"}},
			Stages:  []domain.Stage{{LengthPct: 100, EvictPct: 0}},
		}
	}
	cases := map[string]func(*CreatePlatformExperimentRequest){
		"missing ends_at":   func(r *CreatePlatformExperimentRequest) { r.EndsAt = time.Time{} },
		"no ranking metric": func(r *CreatePlatformExperimentRequest) { r.Metrics = nil },
		"bad stages":        func(r *CreatePlatformExperimentRequest) { r.Stages = []domain.Stage{{LengthPct: 50}} },
		"bad quota tier":    func(r *CreatePlatformExperimentRequest) { r.DefaultQuotaTier = "gold" },
		"bad job policy":    func(r *CreatePlatformExperimentRequest) { r.JobSubmitPolicy = "anyone" },
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
