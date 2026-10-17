package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/scheduler"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"go.uber.org/zap"
)

// environmentRequest is the wire shape of POST /environments.
type environmentRequest struct {
	PlatformExperimentID string `json:"platform_experiment_id,omitempty" required:"false" doc:"The platform experiment whose repository holds the commit. Absent with an agent's token, it is that agent's."`
	CodeRef              string `json:"code_ref" doc:"The commit whose .hypothesisloop/Dockerfile is built: the platform experiment's repository_url@<40-character sha>, as metadata.code_ref of a job."`
	Image                string `json:"image" doc:"The image it is built on: job.image of the job that is to run in it, a job image name or a whole address."`
}

func registerEnvironments(doc *apidocs.Doc, a jobsAPI) {
	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "build-environment", Method: "POST", Path: "/environments",
		Summary: "Build the environment a commit declares, ahead of its job", Tags: []string{"experiments"},
		Description: "Starts building the .hypothesisloop/Dockerfile of commit code_ref on image, the build a job of that commit naming that image waits for (environment_key), so the job finds it built or building when it is submitted. " +
			"Asking again is safe: it answers the one environment as it is then, and builds again one that failed. " +
			"An agent that asks while it builds is told as environment.status on its event stream once a cluster loaded it (loaded: submit its jobs then), or else once the build ends, ready or failed; a failure is told either way. " +
			"422 no_environment_file for a commit without the file, invalid_environment_file for one the platform does not build, no_environment_builder while no cluster the org may use builds, " +
			"unknown_job_image and code_ref_not_in_repository as a submit answers them.",
	}, a.buildEnvironment)
	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "get-environment", Method: "GET", Path: "/environments",
		Summary: "Read the environment a commit declares, building nothing", Tags: []string{"experiments"},
		Description: "Answers the environment the .hypothesisloop/Dockerfile of commit code_ref on image is, as POST /environments answers it, and starts no build: one that failed stays failed. " +
			"An agent that reads it building is told as environment.status once it is loaded or ends, as POST /environments says. " +
			"404 environment_not_requested while nothing asked for its build; 422 no_environment_file and invalid_environment_file as POST /environments answers them.",
	}, a.getEnvironment)
}

func (a jobsAPI) buildEnvironment(ctx context.Context, in *struct{ Body environmentRequest }) (*struct{ Body scheduler.EnvironmentAnswer }, error) {
	caller, ok := auth.FromContext(ctx)
	if !ok {
		return nil, auth.Internal(a.logger, "build environment", scheduler.ErrNoCaller)
	}
	peID, agentID := in.Body.PlatformExperimentID, ""
	if caller.Kind == auth.KindAgent {
		agentID = caller.AgentID
		if peID == "" {
			peID = caller.PlatformExperimentID
		}
	}
	if err := a.access.PlatformExperiment(ctx, peID); err != nil {
		return nil, err
	}
	env, err := a.jobs.BuildEnvironment(ctx, peID, agentID, in.Body.CodeRef, in.Body.Image)
	if err != nil {
		var refused *scheduler.AdmissionError
		if errors.As(err, &refused) {
			return nil, refusal.Refuse(scheduler.AdmissionStatus(refused.Reason), refused.Reason, refused.Message)
		}
		return nil, auth.Internal(a.logger, "build environment", err)
	}
	return &struct{ Body scheduler.EnvironmentAnswer }{Body: scheduler.AnswerEnvironment(env)}, nil
}

func (a jobsAPI) getEnvironment(ctx context.Context, in *struct {
	PlatformExperimentID string `query:"platform_experiment_id" required:"false" doc:"The platform experiment whose repository holds the commit. Absent with an agent's token, it is that agent's."`
	CodeRef              string `query:"code_ref" required:"true" doc:"The commit, as code_ref of POST /environments."`
	Image                string `query:"image" required:"true" doc:"The image it is built on, as image of POST /environments."`
}) (*struct{ Body scheduler.EnvironmentAnswer }, error) {
	caller, ok := auth.FromContext(ctx)
	if !ok {
		return nil, auth.Internal(a.logger, "get environment", scheduler.ErrNoCaller)
	}
	peID, agentID := in.PlatformExperimentID, ""
	if caller.Kind == auth.KindAgent {
		agentID = caller.AgentID
		if peID == "" {
			peID = caller.PlatformExperimentID
		}
	}
	if err := a.access.PlatformExperiment(ctx, peID); err != nil {
		return nil, err
	}
	env, err := a.jobs.FindEnvironment(ctx, peID, agentID, in.CodeRef, in.Image)
	if err != nil {
		var refused *scheduler.AdmissionError
		if errors.As(err, &refused) {
			status := scheduler.AdmissionStatus(refused.Reason)
			if refused.Reason == scheduler.ReasonEnvironmentNotRequested {
				status = http.StatusNotFound
			}
			return nil, refusal.Refuse(status, refused.Reason, refused.Message)
		}
		return nil, auth.Internal(a.logger, "get environment", err)
	}
	return &struct{ Body scheduler.EnvironmentAnswer }{Body: scheduler.AnswerEnvironment(env)}, nil
}

func registerImageCredentials(doc *apidocs.Doc, a jobsAPI) {
	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "create-job-image-credential", Method: "POST", Path: "/experiments/{id}/image-credentials", DefaultStatus: http.StatusCreated,
		Summary: "The image a job runs in, with a login that pulls it", Tags: []string{"experiments"},
		Description: "Answers the image job {id} runs in, so a machine without its hardware can run code in it (hl env stage): the environment its commit declares, with a short-lived login to the registry that holds it, or its job image, pulled without one. " +
			"409 environment_not_ready while that environment builds or is pushed, environment_failed once it failed; 403 run_over once the caller's run has ended.",
	}, a.imageCredential)
}

func (a jobsAPI) imageCredential(ctx context.Context, in *struct {
	ID string `path:"id"`
}) (*struct{ Body *scheduler.ImageCredential }, error) {
	caller, ok := auth.FromContext(ctx)
	if !ok {
		return nil, auth.Internal(a.logger, "job image credential", scheduler.ErrNoCaller)
	}
	if err := a.access.Experiment(ctx, in.ID); err != nil {
		return nil, err
	}
	if caller.RunOver {
		return nil, refusal.Refuse(http.StatusForbidden, "run_over", "the run of this token has ended, so no registry login is handed out for it")
	}
	credential, err := a.jobs.ImageCredential(ctx, in.ID)
	if err != nil {
		var refused *scheduler.AdmissionError
		if errors.As(err, &refused) {
			return nil, refusal.Refuse(http.StatusConflict, refused.Reason, refused.Message)
		}
		return nil, auth.Internal(a.logger, "job image credential", err, zap.String("id", in.ID))
	}
	if credential == nil {
		return nil, huma.Error404NotFound("experiment not found")
	}
	return &struct{ Body *scheduler.ImageCredential }{Body: credential}, nil
}
