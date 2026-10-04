package scheduler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/api"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// ReasonNoEnvironmentFile refuses to build the environment of a commit that declares none.
const ReasonNoEnvironmentFile = "no_environment_file"

// BuildAssigner assigns the build of an environment to a live cluster now (Loop.AssignBuild).
type BuildAssigner interface {
	AssignBuild(ctx context.Context, key string) (*domain.Environment, error)
}

// WithBuildAssigner wires what starts a build asked for ahead of its job.
func (s *Service) WithBuildAssigner(builds BuildAssigner) *Service {
	s.builds = builds
	return s
}

// BuildEnvironment starts the build of the environment commit codeRef declares, on image, for the
// org of platform experiment peID, ahead of the job that will run in it: the one environment, under
// the one key, that a job of that commit naming that image waits for, so the job finds it built
// or building. It returns the environment as it is then: ready, or building, assigned to a live
// cluster that builds for the org and has room, or else by the first tick a job waits for it. A
// failed one is built again, as a submit would. An agent that asks, agentID, is told once the
// build ends (environment.status): it may have no job yet that would be.
func (s *Service) BuildEnvironment(ctx context.Context, peID, agentID, codeRef, image string) (*domain.Environment, error) {
	pe, exp, err := s.environmentJob(ctx, peID, codeRef, image)
	if err != nil {
		return nil, err
	}
	if err := s.declaredEnvironment(ctx, pe, exp); err != nil {
		return nil, err
	}
	if exp.EnvironmentKey == "" {
		return nil, noEnvironmentFile(codeRef)
	}
	if s.builds == nil {
		return nil, errors.New("scheduler: no build assigner is wired (WithBuildAssigner)")
	}
	if agentID != "" {
		if err := s.store.AwaitEnvironmentBuild(ctx, exp.EnvironmentKey, peID, agentID, codeRef, image); err != nil {
			return nil, fmt.Errorf("scheduler: await the build of %s: %w", exp.EnvironmentKey, err)
		}
	}
	env, err := s.builds.AssignBuild(ctx, exp.EnvironmentKey)
	if err != nil {
		return nil, fmt.Errorf("scheduler: assign the build of %s: %w", exp.EnvironmentKey, err)
	}
	if env == nil {
		return nil, fmt.Errorf("scheduler: environment %s is gone", exp.EnvironmentKey)
	}
	return env, nil
}

// ReasonEnvironmentNotRequested answers the state of an environment no build or job asked for.
const ReasonEnvironmentNotRequested = "environment_not_requested"

// FindEnvironment returns the environment commit codeRef declares, on image, for the org of
// platform experiment peID, as it is, and builds nothing: a failed one stays failed. An agent that
// reads one building, agentID, is told once it ends, as one that asked for it is.
func (s *Service) FindEnvironment(ctx context.Context, peID, agentID, codeRef, image string) (*domain.Environment, error) {
	pe, exp, err := s.environmentJob(ctx, peID, codeRef, image)
	if err != nil {
		return nil, err
	}
	commit := codeRef[strings.LastIndex(codeRef, "@")+1:]
	env, declared, err := s.environments.Find(ctx, exp.OrgID, pe.ProjectID, pe.Repository, commit, exp.Job.Image)
	var invalid *domain.EnvironmentFileError
	var refused *api.Refusal
	var unresolved *domain.ImageUnresolvedError
	switch {
	case errors.As(err, &invalid):
		return nil, &AdmissionError{Reason: domain.ReasonInvalidEnvironmentFile, Message: invalid.Why + ". Fix it, commit and build again"}
	case errors.As(err, &refused):
		return nil, &AdmissionError{Reason: ReasonEnvironmentUnreadable, Message: fmt.Sprintf("the platform could not read %s at commit %s: %s", domain.EnvironmentFile, commit, refused.Message)}
	case errors.As(err, &unresolved):
		return nil, &AdmissionError{Reason: ReasonImageUnresolved, Message: fmt.Sprintf("commit %s declares %s, and %s: ask again in a minute", commit, domain.EnvironmentFile, unresolved.Error())}
	case err != nil:
		return nil, fmt.Errorf("scheduler: environment of %s: %w", commit, err)
	case !declared:
		return nil, noEnvironmentFile(codeRef)
	case env == nil:
		return nil, &AdmissionError{Reason: ReasonEnvironmentNotRequested, Message: fmt.Sprintf("the %s of commit %s on this image was never asked to be built: hl env build starts it", domain.EnvironmentFile, commit)}
	}
	if agentID != "" && env.State == domain.EnvironmentBuilding {
		if err := s.store.AwaitEnvironmentBuild(ctx, env.Key, peID, agentID, codeRef, image); err != nil {
			return nil, fmt.Errorf("scheduler: await the build of %s: %w", env.Key, err)
		}
	}
	return env, nil
}

func noEnvironmentFile(codeRef string) error {
	return &AdmissionError{Reason: ReasonNoEnvironmentFile, Message: fmt.Sprintf("commit %s holds no %s, so there is nothing to build: commit the file first", codeRef, domain.EnvironmentFile)}
}

// environmentJob checks that platform experiment peID takes an environment of commit codeRef on
// image now, and returns it with a job of that commit naming image, its image resolved and its
// org set.
func (s *Service) environmentJob(ctx context.Context, peID, codeRef, image string) (*domain.PlatformExperiment, *domain.Experiment, error) {
	pe, err := s.store.GetPlatformExperiment(ctx, peID)
	if err != nil {
		return nil, nil, fmt.Errorf("scheduler: get platform experiment: %w", err)
	}
	if pe == nil {
		return nil, nil, &AdmissionError{Reason: "experiment_not_found", Message: fmt.Sprintf("platform experiment %s not found", peID)}
	}
	if pe.Status != domain.PlatformExpOpen && pe.Status != domain.PlatformExpRunning {
		return nil, nil, &AdmissionError{Reason: "experiment_closed", Message: fmt.Sprintf("platform experiment %s is %s: environments are built for its jobs while it is open or running", pe.ID, pe.Status)}
	}
	if !codeRefPattern.MatchString(codeRef) {
		return nil, nil, &AdmissionError{Reason: ReasonMalformed, Message: "code_ref must be \"<git-remote-url>@<commit-sha>\", a full 40-character commit SHA"}
	}
	if err := s.codeInRepository(pe, codeRef); err != nil {
		return nil, nil, err
	}
	exp := &domain.Experiment{CodeRef: codeRef, Job: domain.JobSpec{Image: image}}
	if err := s.resolveImage(&exp.Job); err != nil {
		return nil, nil, err
	}
	if exp.OrgID, err = s.store.ProjectOrg(ctx, pe.ProjectID); err != nil {
		return nil, nil, fmt.Errorf("scheduler: org of platform experiment: %w", err)
	}
	return pe, exp, nil
}

// environmentRequest is the wire shape of POST /environments.
type environmentRequest struct {
	PlatformExperimentID string `json:"platform_experiment_id,omitempty" required:"false" doc:"The platform experiment whose repository holds the commit. Absent with an agent's token, it is that agent's."`
	CodeRef              string `json:"code_ref" doc:"The commit whose .hypothesisloop/Dockerfile is built: the platform experiment's repository_url@<40-character sha>, as metadata.code_ref of a job."`
	Image                string `json:"image" doc:"The image it is built on: job.image of the job that is to run in it, a job image name or a whole address."`
}

// environmentAnswer is an environment as POST /environments answers it.
type environmentAnswer struct {
	Key     string                  `json:"key" doc:"The environment, as environment_key of every job of the commit that names the image."`
	State   domain.EnvironmentState `json:"state" doc:"building, ready or failed."`
	Base    string                  `json:"base" doc:"The image it is built on."`
	Image   string                  `json:"image,omitempty" doc:"Once ready, the image its jobs run in."`
	Fault   domain.EnvironmentFault `json:"fault,omitempty" doc:"Once failed: file, an instruction of the file failed, or platform."`
	Message string                  `json:"message" doc:"Where the build is, in a sentence, and once the file failed the end of its output."`
}

func answerEnvironment(env *domain.Environment) environmentAnswer {
	answer := environmentAnswer{Key: env.Key, State: env.State, Base: env.Base, Fault: env.Fault}
	switch {
	case env.State == domain.EnvironmentReady:
		answer.Image = env.Image
		answer.Message = "built: a job of this commit on this image runs in " + env.Image
	case env.State == domain.EnvironmentBuilding:
		answer.Message = buildingMessage(env, true)
	case env.Fault == domain.EnvironmentFaultFile:
		answer.Message = fileFailed(env)
	default:
		answer.Message = "the platform could not build it, no fault of the file's: ask again and it is built again"
	}
	return answer
}

func registerEnvironments(doc *apidocs.Doc, h *Handler) {
	access := doc.Access
	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "build-environment", Method: "POST", Path: "/environments",
		Summary: "Build the environment a commit declares, ahead of its job", Tags: []string{"experiments"},
		Description: "Starts building the .hypothesisloop/Dockerfile of commit code_ref on image, the build a job of that commit naming that image waits for (environment_key), so the job finds it built or building when it is submitted. " +
			"Asking again is safe: it answers the one environment as it is then, and builds again one that failed. " +
			"An agent that asks while it builds is told once the build ends, ready or failed, as environment.status on its event stream. " +
			"422 no_environment_file for a commit without the file, invalid_environment_file for one the platform does not build, no_environment_builder while no cluster the org may use builds, " +
			"unknown_job_image and code_ref_not_in_repository as a submit answers them.",
	}, func(ctx context.Context, in *struct{ Body environmentRequest }) (*struct{ Body environmentAnswer }, error) {
		caller, ok := auth.FromContext(ctx)
		if !ok {
			return nil, auth.Internal(h.svc.logger, "build environment", errNoCaller)
		}
		peID, agentID := in.Body.PlatformExperimentID, ""
		if caller.Kind == auth.KindAgent {
			agentID = caller.AgentID
			if peID == "" {
				peID = caller.PlatformExperimentID
			}
		}
		if err := access.PlatformExperiment(ctx, peID); err != nil {
			return nil, err
		}
		env, err := h.svc.BuildEnvironment(ctx, peID, agentID, in.Body.CodeRef, in.Body.Image)
		if err != nil {
			var refused *AdmissionError
			if errors.As(err, &refused) {
				return nil, api.Refuse(admissionHTTPStatus(refused.Reason), refused.Reason, refused.Message)
			}
			return nil, auth.Internal(h.svc.logger, "build environment", err)
		}
		return &struct{ Body environmentAnswer }{Body: answerEnvironment(env)}, nil
	})
	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "get-environment", Method: "GET", Path: "/environments",
		Summary: "Read the environment a commit declares, building nothing", Tags: []string{"experiments"},
		Description: "Answers the environment the .hypothesisloop/Dockerfile of commit code_ref on image is, as POST /environments answers it, and starts no build: one that failed stays failed. " +
			"An agent that reads it building is told once the build ends, as environment.status. " +
			"404 environment_not_requested while nothing asked for its build; 422 no_environment_file and invalid_environment_file as POST /environments answers them.",
	}, func(ctx context.Context, in *struct {
		PlatformExperimentID string `query:"platform_experiment_id" required:"false" doc:"The platform experiment whose repository holds the commit. Absent with an agent's token, it is that agent's."`
		CodeRef              string `query:"code_ref" required:"true" doc:"The commit, as code_ref of POST /environments."`
		Image                string `query:"image" required:"true" doc:"The image it is built on, as image of POST /environments."`
	}) (*struct{ Body environmentAnswer }, error) {
		caller, ok := auth.FromContext(ctx)
		if !ok {
			return nil, auth.Internal(h.svc.logger, "get environment", errNoCaller)
		}
		peID, agentID := in.PlatformExperimentID, ""
		if caller.Kind == auth.KindAgent {
			agentID = caller.AgentID
			if peID == "" {
				peID = caller.PlatformExperimentID
			}
		}
		if err := access.PlatformExperiment(ctx, peID); err != nil {
			return nil, err
		}
		env, err := h.svc.FindEnvironment(ctx, peID, agentID, in.CodeRef, in.Image)
		if err != nil {
			var refused *AdmissionError
			if errors.As(err, &refused) {
				status := admissionHTTPStatus(refused.Reason)
				if refused.Reason == ReasonEnvironmentNotRequested {
					status = http.StatusNotFound
				}
				return nil, api.Refuse(status, refused.Reason, refused.Message)
			}
			return nil, auth.Internal(h.svc.logger, "get environment", err)
		}
		return &struct{ Body environmentAnswer }{Body: answerEnvironment(env)}, nil
	})
}
