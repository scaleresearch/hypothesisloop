package httpapi

import (
	"context"

	"github.com/danielgtaylor/huma/v2"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/platformexperiments"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
)

type sharingAPI struct {
	sharing *platformexperiments.Sharing
	answer  func(context.Context, *domain.PlatformExperiment) (*struct{ Body *domain.PlatformExperiment }, error)
}

// RegisterSharing registers how staff share a platform experiment with every account and take
// that back. The staff audience is the whole rule: what sharing lets a caller reach is
// auth.Access's. A platform experiment is answered as its own routes answer it.
func RegisterSharing(doc *apidocs.Doc, sharing *platformexperiments.Sharing, experiments *platformexperiments.PlatformExperimentsService, logger *zap.Logger) {
	registerSharing(doc, sharing, platformExperimentsAPI{experiments: experiments, access: doc.Access, logger: logger}.answer)
}

func registerSharing(doc *apidocs.Doc, sharing *platformexperiments.Sharing, answer func(context.Context, *domain.PlatformExperiment) (*struct{ Body *domain.PlatformExperiment }, error)) {
	a := sharingAPI{sharing: sharing, answer: answer}
	apidocs.Register(doc, apidocs.AudienceStaff, huma.Operation{
		OperationID: "share-platform-experiment", Method: "POST", Path: "/platform-experiments/{id}/share",
		Summary: "Show a platform experiment to every account", Tags: []string{"platform-experiments"},
		Description: "Staff only. Every account then reads the platform experiment and everything in it through the same read routes as its own: " +
			"its agents, jobs, hypotheses, findings, comments, metrics, logs, data listings, results and its watch stream. Nobody outside its org writes to it " +
			"or is handed a credential of it, and GET /platform-experiments lists it for every account beside the account's own. " +
			"What its jobs and agents wrote is shown as it is, so share only what may be read by anyone. Sharing a shared one changes nothing.",
	}, a.share)
	apidocs.Register(doc, apidocs.AudienceStaff, huma.Operation{
		OperationID: "unshare-platform-experiment", Method: "POST", Path: "/platform-experiments/{id}/unshare",
		Summary: "Stop showing a platform experiment to every account", Tags: []string{"platform-experiments"},
		Description: "Staff only. From the next request on the platform experiment is read by its own org alone, and an open watch stream of another org's caller ends at its next check.",
	}, a.unshare)
}

func (a sharingAPI) share(ctx context.Context, in *struct {
	ID string `path:"id"`
}) (*struct{ Body *domain.PlatformExperiment }, error) {
	return a.set(ctx, in.ID, true)
}

func (a sharingAPI) unshare(ctx context.Context, in *struct {
	ID string `path:"id"`
}) (*struct{ Body *domain.PlatformExperiment }, error) {
	return a.set(ctx, in.ID, false)
}

func (a sharingAPI) set(ctx context.Context, id string, shared bool) (*struct{ Body *domain.PlatformExperiment }, error) {
	pe, err := a.sharing.SetShared(ctx, id, shared)
	if err != nil {
		return nil, err
	}
	return a.answer(ctx, pe)
}
