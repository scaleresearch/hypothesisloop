package platformexperiments

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
)

// Sharing is the persistence of the platform's decision to show a platform experiment to every
// account. *db.PlatformExperimentsStore satisfies it.
type Sharing interface {
	SetPlatformExperimentShared(ctx context.Context, id string, shared bool) (found bool, err error)
	GetPlatformExperiment(ctx context.Context, id string) (*domain.PlatformExperiment, error)
}

// PlatformExperimentAnswer is how a route answers one platform experiment:
// PlatformExperimentsHandler.Answer.
type PlatformExperimentAnswer func(context.Context, *domain.PlatformExperiment) (*struct{ Body *domain.PlatformExperiment }, error)

// RegisterShareHuma registers how staff share a platform experiment with every account and take
// that back. The staff audience is the whole rule: what sharing lets a caller reach is
// auth.Access's.
func RegisterShareHuma(doc *apidocs.Doc, store Sharing, answer PlatformExperimentAnswer, logger *zap.Logger) {
	failed := func(id string, err error) error {
		return auth.Internal(logger, "sharing a platform experiment failed", err, zap.String("platform_experiment_id", id))
	}
	set := func(shared bool) func(context.Context, *struct {
		ID string `path:"id"`
	}) (*struct{ Body *domain.PlatformExperiment }, error) {
		return func(ctx context.Context, in *struct {
			ID string `path:"id"`
		}) (*struct{ Body *domain.PlatformExperiment }, error) {
			missing := refusal.Refuse(http.StatusNotFound, "not_found", "platform experiment not found")
			if !domain.IdentifierShaped(in.ID) {
				return nil, missing
			}
			found, err := store.SetPlatformExperimentShared(ctx, in.ID, shared)
			if err != nil {
				return nil, failed(in.ID, err)
			}
			if !found {
				return nil, missing
			}
			pe, err := store.GetPlatformExperiment(ctx, in.ID)
			if err != nil {
				return nil, failed(in.ID, err)
			}
			if pe == nil {
				return nil, missing
			}
			return answer(ctx, pe)
		}
	}
	apidocs.Register(doc, apidocs.AudienceStaff, huma.Operation{
		OperationID: "share-platform-experiment", Method: "POST", Path: "/platform-experiments/{id}/share",
		Summary: "Show a platform experiment to every account", Tags: []string{"platform-experiments"},
		Description: "Staff only. Every account then reads the platform experiment and everything in it through the same read routes as its own: " +
			"its agents, jobs, hypotheses, findings, comments, metrics, logs, data listings, results and its watch stream. Nobody outside its org writes to it " +
			"or is handed a credential of it, and GET /platform-experiments lists it for every account beside the account's own. " +
			"What its jobs and agents wrote is shown as it is, so share only what may be read by anyone. Sharing a shared one changes nothing.",
	}, set(true))
	apidocs.Register(doc, apidocs.AudienceStaff, huma.Operation{
		OperationID: "unshare-platform-experiment", Method: "POST", Path: "/platform-experiments/{id}/unshare",
		Summary: "Stop showing a platform experiment to every account", Tags: []string{"platform-experiments"},
		Description: "Staff only. From the next request on the platform experiment is read by its own org alone, and an open watch stream of another org's caller ends at its next check.",
	}, set(false))
}
