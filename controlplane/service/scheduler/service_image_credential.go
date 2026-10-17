package scheduler

import (
	"context"
	"errors"
	"fmt"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// RegistryPulls hands out logins that pull environment images (registrycred.Source).
type RegistryPulls interface {
	Pull(ctx context.Context) (domain.RegistryLogin, error)
}

// WithRegistryPulls wires the logins that pull environment images, on a deployment that builds them.
func (s *Service) WithRegistryPulls(pulls RegistryPulls) *Service {
	s.pulls = pulls
	return s
}

// ReasonEnvironmentNotReady refuses the image of a job whose environment is still building, and
// ReasonEnvironmentFailed one whose environment failed: the first is worth waiting on, the second
// is not.
const (
	ReasonEnvironmentNotReady = "environment_not_ready"
	ReasonEnvironmentFailed   = "environment_failed"
)

// ImageCredential is the image job id runs in, and a login that pulls it when it is an
// environment the platform built. A job whose commit declares no environment runs in its job
// image, which is pulled without one, as a node pulls it.
func (s *Service) ImageCredential(ctx context.Context, id string) (*ImageCredential, error) {
	exp, err := s.store.GetExperiment(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("scheduler: get job %s: %w", id, err)
	}
	if exp == nil {
		return nil, nil
	}
	if exp.EnvironmentKey == "" {
		return &ImageCredential{Image: exp.Job.Image}, nil
	}
	env, err := s.store.GetEnvironment(ctx, exp.EnvironmentKey)
	if err != nil {
		return nil, fmt.Errorf("scheduler: environment of job %s: %w", id, err)
	}
	if env == nil {
		return nil, fmt.Errorf("scheduler: job %s names environment %s, which does not exist", id, exp.EnvironmentKey)
	}
	if env.State != domain.EnvironmentReady {
		answer := AnswerEnvironment(env)
		reason := ReasonEnvironmentNotReady
		if env.State == domain.EnvironmentFailed {
			reason = ReasonEnvironmentFailed
		}
		return nil, &AdmissionError{Reason: reason, Message: fmt.Sprintf("job %s runs in environment %s, which is %s: %s", id, env.Key, answer.State, answer.Message)}
	}
	if s.pulls == nil {
		return nil, errors.New("scheduler: an environment is ready and no registry logins are wired (WithRegistryPulls)")
	}
	login, err := s.pulls.Pull(ctx)
	if err != nil {
		return nil, fmt.Errorf("scheduler: pull login: %w", err)
	}
	return &ImageCredential{Image: env.Image, EnvironmentKey: env.Key, Login: &login}, nil
}

// ImageCredential is the answer of POST /experiments/{id}/image-credentials.
type ImageCredential struct {
	Image          string                `json:"image" doc:"The image the job runs in: its environment's once it declares one, else job.image."`
	EnvironmentKey string                `json:"environment_key,omitempty" doc:"The environment image is, absent for a job that declares none."`
	Login          *domain.RegistryLogin `json:"login,omitempty" doc:"A login to the registry host registry that pulls image, good until expires_at. Absent for a job image, which is pulled anonymously."`
}
