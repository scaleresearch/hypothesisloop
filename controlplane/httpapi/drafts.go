package httpapi

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/platformexperiments"
)

type experimentDraftRequest struct {
	Form    json.RawMessage `json:"form" doc:"The form as the client holds it: a JSON object, stored and returned unread."`
	Step    string          `json:"step" doc:"The step of the form the person was on."`
	Failure string          `json:"failure,omitempty" required:"false" doc:"Why the last create from this draft was refused. Left out, none was."`
}

func (b *experimentDraftRequest) form() ([]byte, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(b.Form, &object); err != nil || object == nil {
		return nil, huma.Error400BadRequest("form must be a JSON object")
	}
	return b.Form, nil
}

const draftsDescription = "A draft is a new platform experiment's form as a person left it, kept in the project for anyone who reaches it to finish. " +
	"The platform reads nothing in it: form is the client's own, and the create that finishes a draft checks everything. failure is why the last create from it was refused. " +
	"A draft is not a platform experiment and runs nothing. At most 256 KiB (413 above). 404 not_found for a draft the project does not hold."

type draftsAPI struct {
	drafts *platformexperiments.Drafts
	access *auth.Access
}

// registerExperimentDrafts registers the drafts a project keeps of new platform experiments.
func registerExperimentDrafts(doc *Doc, drafts *platformexperiments.Drafts) {
	a := draftsAPI{drafts: drafts, access: doc.Access}
	register(doc, auth.AudienceCoordinator, huma.Operation{
		OperationID: "create-experiment-draft", Method: "POST", Path: "/projects/{id}/experiment-drafts", DefaultStatus: http.StatusCreated,
		Summary: "Keep a new platform experiment's form as a draft", Tags: []string{"platform-experiments"},
		MaxBodyBytes: domain.MaxExperimentDraftBytes, Description: draftsDescription,
	}, a.create)
	register(doc, auth.AudienceCoordinator, huma.Operation{
		OperationID: "list-experiment-drafts", Method: "GET", Path: "/projects/{id}/experiment-drafts",
		Summary: "List a project's drafts of new platform experiments", Tags: []string{"platform-experiments"},
		Description: draftsDescription + " The one changed last comes first.",
	}, a.list)
	register(doc, auth.AudienceCoordinator, huma.Operation{
		OperationID: "get-experiment-draft", Method: "GET", Path: "/projects/{id}/experiment-drafts/{draft_id}",
		Summary: "Read a draft of a new platform experiment", Tags: []string{"platform-experiments"}, Description: draftsDescription,
	}, a.get)
	register(doc, auth.AudienceCoordinator, huma.Operation{
		OperationID: "put-experiment-draft", Method: "PUT", Path: "/projects/{id}/experiment-drafts/{draft_id}",
		Summary: "Replace what a draft of a new platform experiment holds", Tags: []string{"platform-experiments"},
		MaxBodyBytes: domain.MaxExperimentDraftBytes, Description: draftsDescription,
	}, a.put)
	register(doc, auth.AudienceCoordinator, huma.Operation{
		OperationID: "delete-experiment-draft", Method: "DELETE", Path: "/projects/{id}/experiment-drafts/{draft_id}", DefaultStatus: http.StatusNoContent,
		Summary: "Delete a draft of a new platform experiment", Tags: []string{"platform-experiments"}, Description: draftsDescription,
	}, a.delete)
}

func (a draftsAPI) create(ctx context.Context, in *struct {
	ID   string `path:"id"`
	Body experimentDraftRequest
}) (*struct{ Body *domain.ExperimentDraft }, error) {
	if err := a.access.Project(ctx, in.ID); err != nil {
		return nil, err
	}
	form, err := in.Body.form()
	if err != nil {
		return nil, err
	}
	return answerDraft(a.drafts.Create(ctx, in.ID, form, in.Body.Step, in.Body.Failure))
}

func (a draftsAPI) list(ctx context.Context, in *struct {
	ID string `path:"id"`
}) (*struct{ Body []*domain.ExperimentDraft }, error) {
	if err := a.access.Project(ctx, in.ID); err != nil {
		return nil, err
	}
	list, err := a.drafts.List(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	return &struct{ Body []*domain.ExperimentDraft }{Body: list}, nil
}

func (a draftsAPI) get(ctx context.Context, in *struct {
	ID      string `path:"id"`
	DraftID string `path:"draft_id"`
}) (*struct{ Body *domain.ExperimentDraft }, error) {
	if err := a.access.Project(ctx, in.ID); err != nil {
		return nil, err
	}
	return answerDraft(a.drafts.Get(ctx, in.ID, in.DraftID))
}

func (a draftsAPI) put(ctx context.Context, in *struct {
	ID      string `path:"id"`
	DraftID string `path:"draft_id"`
	Body    experimentDraftRequest
}) (*struct{ Body *domain.ExperimentDraft }, error) {
	if err := a.access.Project(ctx, in.ID); err != nil {
		return nil, err
	}
	form, err := in.Body.form()
	if err != nil {
		return nil, err
	}
	return answerDraft(a.drafts.Update(ctx, in.ID, in.DraftID, form, in.Body.Step, in.Body.Failure))
}

func (a draftsAPI) delete(ctx context.Context, in *struct {
	ID      string `path:"id"`
	DraftID string `path:"draft_id"`
}) (*struct{}, error) {
	if err := a.access.Project(ctx, in.ID); err != nil {
		return nil, err
	}
	if err := a.drafts.Delete(ctx, in.ID, in.DraftID); err != nil {
		return nil, err
	}
	return &struct{}{}, nil
}

func answerDraft(d *domain.ExperimentDraft, err error) (*struct{ Body *domain.ExperimentDraft }, error) {
	if err != nil {
		return nil, err
	}
	return &struct{ Body *domain.ExperimentDraft }{Body: d}, nil
}
