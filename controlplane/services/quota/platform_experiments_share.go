package quota

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// Sharing is the persistence of the platform's decision to show a platform experiment to every
// account. *db.PlatformExperimentsStore satisfies it.
type Sharing interface {
	SetPlatformExperimentShared(ctx context.Context, id string, shared bool) (found bool, err error)
	GetPlatformExperiment(ctx context.Context, id string) (*domain.PlatformExperiment, error)
}

type shareRefusal struct {
	status  int
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

func (e *shareRefusal) Error() string  { return e.Message }
func (e *shareRefusal) GetStatus() int { return e.status }

// RegisterShareHuma registers how staff share a platform experiment with every account and take
// that back. The staff audience is the whole rule: what sharing lets a caller reach is
// auth.Access's.
func RegisterShareHuma(doc *apidocs.Doc, store Sharing, logger *zap.Logger) {
	failed := func(id string, err error) error {
		logger.Error("sharing a platform experiment failed", zap.String("platform_experiment_id", id), zap.Error(err))
		return &shareRefusal{status: http.StatusInternalServerError, Reason: "internal", Message: auth.InternalError}
	}
	set := func(shared bool) func(context.Context, *struct {
		ID string `path:"id"`
	}) (*struct{ Body *domain.PlatformExperiment }, error) {
		return func(ctx context.Context, in *struct {
			ID string `path:"id"`
		}) (*struct{ Body *domain.PlatformExperiment }, error) {
			missing := &shareRefusal{status: http.StatusNotFound, Reason: "not_found", Message: "platform experiment not found"}
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
			return &struct{ Body *domain.PlatformExperiment }{Body: pe}, nil
		}
	}
	apidocs.Register(doc, apidocs.AudienceStaff, huma.Operation{
		OperationID: "share-platform-experiment", Method: "POST", Path: "/platform-experiments/{id}/share",
		Summary: "Show a platform experiment to every account", Tags: []string{"platform-experiments"},
		Description: "Staff only. Every account then reads the platform experiment and everything in it through the same read routes as its own: " +
			"its agents, jobs, hypotheses, findings, comments, metrics, logs, data listings, results and its watch stream. Nobody outside its org writes to it " +
			"or is handed a credential of it, and it appears in no listing but GET /platform-experiments?shared=true. " +
			"What its jobs and agents wrote is shown as it is, so share only what may be read by anyone. Sharing a shared one changes nothing.",
	}, set(true))
	apidocs.Register(doc, apidocs.AudienceStaff, huma.Operation{
		OperationID: "unshare-platform-experiment", Method: "POST", Path: "/platform-experiments/{id}/unshare",
		Summary: "Stop showing a platform experiment to every account", Tags: []string{"platform-experiments"},
		Description: "Staff only. From the next request on the platform experiment is read by its own org alone, and an open watch stream of another org's caller ends at its next check.",
	}, set(false))
}
