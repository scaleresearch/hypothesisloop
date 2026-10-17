package scheduler

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/api"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
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
		answer := answerEnvironment(env)
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

func registerImageCredentials(doc *apidocs.Doc, h *Handler) {
	access := doc.Access
	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "create-job-image-credential", Method: "POST", Path: "/experiments/{id}/image-credentials", DefaultStatus: http.StatusCreated,
		Summary: "The image a job runs in, with a login that pulls it", Tags: []string{"experiments"},
		Description: "Answers the image job {id} runs in, so a machine without its hardware can run code in it (hl env stage): the environment its commit declares, with a short-lived login to the registry that holds it, or its job image, pulled without one. " +
			"409 environment_not_ready while that environment builds or is pushed, environment_failed once it failed; 403 run_over once the caller's run has ended.",
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*struct{ Body *ImageCredential }, error) {
		caller, ok := auth.FromContext(ctx)
		if !ok {
			return nil, auth.Internal(h.svc.logger, "job image credential", errNoCaller)
		}
		if err := access.Experiment(ctx, in.ID); err != nil {
			return nil, err
		}
		if caller.RunOver {
			return nil, api.Refuse(http.StatusForbidden, "run_over", "the run of this token has ended, so no registry login is handed out for it")
		}
		credential, err := h.svc.ImageCredential(ctx, in.ID)
		if err != nil {
			var refused *AdmissionError
			if errors.As(err, &refused) {
				return nil, api.Refuse(http.StatusConflict, refused.Reason, refused.Message)
			}
			return nil, auth.Internal(h.svc.logger, "job image credential", err, zap.String("id", in.ID))
		}
		if credential == nil {
			return nil, huma.Error404NotFound("experiment not found")
		}
		return &struct{ Body *ImageCredential }{Body: credential}, nil
	})
}
