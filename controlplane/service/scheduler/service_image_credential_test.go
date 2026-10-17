package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/contract/platformapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// jobsAndEnvironments holds one job and the environments it may name.
type jobsAndEnvironments struct {
	poolOnlyStore
	job  *domain.Experiment
	envs map[string]*domain.Environment
}

func (s jobsAndEnvironments) GetExperiment(_ context.Context, id string) (*domain.Experiment, error) {
	if s.job != nil && s.job.ID == id {
		return s.job, nil
	}
	return nil, nil
}

func (s jobsAndEnvironments) GetEnvironment(_ context.Context, key string) (*domain.Environment, error) {
	return s.envs[key], nil
}

type pullLogin struct{ asked *int }

func (p pullLogin) Pull(context.Context) (domain.RegistryLogin, error) {
	*p.asked++
	return domain.RegistryLogin{Registry: "registry.test", Username: "oauth2accesstoken", Password: "secret", ExpiresAt: time.Unix(1, 0)}, nil
}

// A job runs in its environment once that is ready, pulled with a login, and in its job image when
// its commit declares none, pulled as a node pulls it, without one. An environment that is not
// ready has no image to pull yet.
func TestAJobsImageIsItsReadyEnvironmentOrItsJobImage(t *testing.T) {
	envs := map[string]*domain.Environment{
		"ready":    {Key: "ready", State: domain.EnvironmentReady, Image: "registry.test/environment@sha256:1"},
		"building": {Key: "building", State: domain.EnvironmentBuilding, ClusterName: "builder-a", Attempt: 1},
		"failed":   {Key: "failed", State: domain.EnvironmentFailed, Fault: domain.EnvironmentFaultPlatform},
	}
	asked := 0
	of := func(key string) *Service {
		job := &domain.Experiment{ID: "job-1", EnvironmentKey: key, Job: domain.JobSpec{Image: "registry.test/job:1"}}
		return &Service{store: jobsAndEnvironments{job: job, envs: envs}, pulls: pullLogin{&asked}}
	}

	got, err := of("ready").ImageCredential(context.Background(), "job-1")
	if err != nil || got.Image != "registry.test/environment@sha256:1" || got.EnvironmentKey != "ready" || got.Login == nil || got.Login.Registry != "registry.test" || asked != 1 {
		t.Errorf("ready: got %+v, %v, want the environment's image with a login", got, err)
	}
	got, err = of("").ImageCredential(context.Background(), "job-1")
	if err != nil || got.Image != "registry.test/job:1" || got.Login != nil || asked != 1 {
		t.Errorf("no environment: got %+v, %v, want the job image and no login", got, err)
	}
	for key, reason := range map[string]string{"building": platformapi.ReasonEnvironmentNotReady, "failed": ReasonEnvironmentFailed} {
		var refused *AdmissionError
		if _, err := of(key).ImageCredential(context.Background(), "job-1"); !errors.As(err, &refused) || refused.Reason != reason {
			t.Errorf("%s: got %v, want %s", key, err, reason)
		}
	}
	if got, err := of("ready").ImageCredential(context.Background(), "job-2"); got != nil || err != nil {
		t.Errorf("another job: got %+v, %v, want none", got, err)
	}
}
