package httpapi

import (
	"context"

	"github.com/danielgtaylor/huma/v2"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/platformexperiments"
)

type experimentImageAPI struct {
	experiments *platformexperiments.Service
	access      *auth.Access
	logger      *zap.Logger
}

// registerExperimentImage registers the image a platform experiment's jobs run in and the logins
// its images are pushed with.
func registerExperimentImage(doc *Doc, experiments *platformexperiments.Service, logger *zap.Logger) {
	a := experimentImageAPI{experiments: experiments, access: doc.Access, logger: logger}
	register(doc, auth.AudienceAgent, huma.Operation{
		OperationID: "set-platform-experiment-image", Method: "PUT", Path: "/platform-experiments/{id}/image",
		Summary: "Record the image the platform experiment's jobs run in", Tags: []string{"platform-experiments"},
		Description: "image is any image a node pulls: a public one, or one pushed to this platform experiment's repository (POST /platform-experiments/{id}/registry-credentials). " +
			"The platform asks its registry which image the reference names and records it by digest, with the architecture it runs on: architecture, " +
			"or the one the image runs on when that is one; 422 image_architecture names them when it runs on several. Every job submitted from then on runs in it, " +
			"on nodes of that architecture, and records the digest; a job submitted before keeps its own. It needs sh and git: each job's commit is fetched into " +
			domain.JobCodeDir + " at start and its command runs there. Recorded while the platform experiment is open or running. " +
			"422 image_unresolved when the registry does not answer for it, image_of_another_org for one in another org's repository.",
	}, a.set)
	register(doc, auth.AudienceAgent, huma.Operation{
		OperationID: "get-registry-credentials", Method: "POST", Path: "/platform-experiments/{id}/registry-credentials",
		Summary: "Get a login that pushes the platform experiment's images", Tags: []string{"platform-experiments"},
		Description: "A short-lived login to registry that pushes to and pulls from the repository of the platform experiment's org, good until expires_at, at least 25 minutes. " +
			"Tag an image <repository>:<short commit>-<label>, push it there directly and record it with PUT /platform-experiments/{id}/image. " +
			"Nothing of the image passes through the platform. 409 no_registry on a deployment that keeps none. `hl image credentials` prints it as environment variables.",
	}, a.credentials)
}

type setImageRequest struct {
	Image        string `json:"image" minLength:"1" doc:"The image, by tag or digest, registry host first for one not on Docker Hub."`
	Architecture string `json:"architecture,omitempty" doc:"The architecture its jobs run on, one the image runs on. Needed only for an image that runs on several."`
}

// writer admits the caller to change platform experiment id while its run goes on.
func (a experimentImageAPI) writer(ctx context.Context, id string) error {
	if err := a.access.PlatformExperiment(ctx, id); err != nil {
		return err
	}
	writes, err := a.access.Writes(ctx, id)
	if err != nil {
		return auth.Internal(a.logger, "platform experiment image", err, zap.String("id", id))
	}
	if !writes {
		return auth.Forbidden("read-only: this token reads platform experiment " + id + " and changes nothing of it")
	}
	if p, ok := auth.FromContext(ctx); ok && p.RunOver {
		return auth.Forbidden("run over: the run of this " + string(p.Kind) + " token in platform experiment " + id + " has ended")
	}
	return nil
}

func (a experimentImageAPI) set(ctx context.Context, in *struct {
	ID   string `path:"id"`
	Body setImageRequest
}) (*struct{ Body *domain.ExperimentImage }, error) {
	if err := a.writer(ctx, in.ID); err != nil {
		return nil, err
	}
	image, err := a.experiments.SetImage(ctx, in.ID, in.Body.Image, in.Body.Architecture)
	if err != nil {
		return nil, refused(a.logger, "set platform experiment image", err)
	}
	return &struct{ Body *domain.ExperimentImage }{Body: image}, nil
}

func (a experimentImageAPI) credentials(ctx context.Context, in *struct {
	ID string `path:"id"`
}) (*struct{ Body *domain.RegistryCredential }, error) {
	if err := a.writer(ctx, in.ID); err != nil {
		return nil, err
	}
	credential, err := a.experiments.RegistryCredential(ctx, in.ID)
	if err != nil {
		return nil, refused(a.logger, "get registry credentials", err)
	}
	return &struct{ Body *domain.RegistryCredential }{Body: credential}, nil
}
