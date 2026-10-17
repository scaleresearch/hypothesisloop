package quota

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
)

// ExperimentDraftStore keeps the forms of new platform experiments people left to finish later.
// *db.ExperimentDraftsStore is the one in use.
type ExperimentDraftStore interface {
	CreateExperimentDraft(ctx context.Context, projectID string, form []byte, step, failure string) (*domain.ExperimentDraft, error)
	ListExperimentDrafts(ctx context.Context, projectID string) ([]*domain.ExperimentDraft, error)
	GetExperimentDraft(ctx context.Context, projectID, id string) (*domain.ExperimentDraft, error)
	UpdateExperimentDraft(ctx context.Context, projectID, id string, form []byte, step, failure string) (*domain.ExperimentDraft, error)
	DeleteExperimentDraft(ctx context.Context, projectID, id string) error
}

type experimentDraftBody struct {
	Form    json.RawMessage `json:"form" doc:"The form as the client holds it: a JSON object, stored and returned unread."`
	Step    string          `json:"step" doc:"The step of the form the person was on."`
	Failure string          `json:"failure,omitempty" required:"false" doc:"Why the last create from this draft was refused. Left out, none was."`
}

func (b *experimentDraftBody) form() ([]byte, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(b.Form, &object); err != nil || object == nil {
		return nil, huma.Error400BadRequest("form must be a JSON object")
	}
	return b.Form, nil
}

const draftsDescription = "A draft is a new platform experiment's form as a person left it, kept in the project for anyone who reaches it to finish. " +
	"The platform reads nothing in it: form is the client's own, and the create that finishes a draft checks everything. failure is why the last create from it was refused. " +
	"A draft is not a platform experiment and runs nothing. At most 256 KiB (413 above). 404 not_found for a draft the project does not hold."

// RegisterExperimentDraftsHuma registers the drafts a project keeps of new platform experiments.
func RegisterExperimentDraftsHuma(doc *apidocs.Doc, drafts ExperimentDraftStore, logger *zap.Logger) {
	access := doc.Access
	answer := func(d *domain.ExperimentDraft, err error, what string) (*struct{ Body *domain.ExperimentDraft }, error) {
		if errors.Is(err, db.ErrExperimentDraftNotFound) {
			return nil, auth.NotFound("experiment draft")
		}
		if err != nil {
			return nil, auth.Internal(logger, what, err)
		}
		return &struct{ Body *domain.ExperimentDraft }{Body: d}, nil
	}

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "create-experiment-draft", Method: "POST", Path: "/projects/{id}/experiment-drafts", DefaultStatus: http.StatusCreated,
		Summary: "Keep a new platform experiment's form as a draft", Tags: []string{"platform-experiments"},
		MaxBodyBytes: domain.MaxExperimentDraftBytes, Description: draftsDescription,
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Body experimentDraftBody
	}) (*struct{ Body *domain.ExperimentDraft }, error) {
		if err := access.Project(ctx, in.ID); err != nil {
			return nil, err
		}
		form, err := in.Body.form()
		if err != nil {
			return nil, err
		}
		d, err := drafts.CreateExperimentDraft(ctx, in.ID, form, in.Body.Step, in.Body.Failure)
		return answer(d, err, "create experiment draft")
	})
	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "list-experiment-drafts", Method: "GET", Path: "/projects/{id}/experiment-drafts",
		Summary: "List a project's drafts of new platform experiments", Tags: []string{"platform-experiments"},
		Description: draftsDescription + " The one changed last comes first.",
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*struct{ Body []*domain.ExperimentDraft }, error) {
		if err := access.Project(ctx, in.ID); err != nil {
			return nil, err
		}
		list, err := drafts.ListExperimentDrafts(ctx, in.ID)
		if err != nil {
			return nil, auth.Internal(logger, "list experiment drafts", err)
		}
		return &struct{ Body []*domain.ExperimentDraft }{Body: list}, nil
	})
	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "get-experiment-draft", Method: "GET", Path: "/projects/{id}/experiment-drafts/{draft_id}",
		Summary: "Read a draft of a new platform experiment", Tags: []string{"platform-experiments"}, Description: draftsDescription,
	}, func(ctx context.Context, in *struct {
		ID      string `path:"id"`
		DraftID string `path:"draft_id"`
	}) (*struct{ Body *domain.ExperimentDraft }, error) {
		if err := access.Project(ctx, in.ID); err != nil {
			return nil, err
		}
		d, err := drafts.GetExperimentDraft(ctx, in.ID, in.DraftID)
		return answer(d, err, "read experiment draft")
	})
	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "put-experiment-draft", Method: "PUT", Path: "/projects/{id}/experiment-drafts/{draft_id}",
		Summary: "Replace what a draft of a new platform experiment holds", Tags: []string{"platform-experiments"},
		MaxBodyBytes: domain.MaxExperimentDraftBytes, Description: draftsDescription,
	}, func(ctx context.Context, in *struct {
		ID      string `path:"id"`
		DraftID string `path:"draft_id"`
		Body    experimentDraftBody
	}) (*struct{ Body *domain.ExperimentDraft }, error) {
		if err := access.Project(ctx, in.ID); err != nil {
			return nil, err
		}
		form, err := in.Body.form()
		if err != nil {
			return nil, err
		}
		d, err := drafts.UpdateExperimentDraft(ctx, in.ID, in.DraftID, form, in.Body.Step, in.Body.Failure)
		return answer(d, err, "update experiment draft")
	})
	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "delete-experiment-draft", Method: "DELETE", Path: "/projects/{id}/experiment-drafts/{draft_id}", DefaultStatus: http.StatusNoContent,
		Summary: "Delete a draft of a new platform experiment", Tags: []string{"platform-experiments"}, Description: draftsDescription,
	}, func(ctx context.Context, in *struct {
		ID      string `path:"id"`
		DraftID string `path:"draft_id"`
	}) (*struct{}, error) {
		if err := access.Project(ctx, in.ID); err != nil {
			return nil, err
		}
		err := drafts.DeleteExperimentDraft(ctx, in.ID, in.DraftID)
		if errors.Is(err, db.ErrExperimentDraftNotFound) {
			return nil, auth.NotFound("experiment draft")
		}
		if err != nil {
			return nil, auth.Internal(logger, "delete experiment draft", err)
		}
		return &struct{}{}, nil
	})
}
