package platformexperiments

import (
	"context"
	"errors"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
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

// Drafts keeps the drafts a project holds of new platform experiments. A draft the project does
// not hold is answered as not found.
type Drafts struct {
	store  ExperimentDraftStore
	logger *zap.Logger
}

func NewDrafts(store ExperimentDraftStore, logger *zap.Logger) *Drafts {
	return &Drafts{store: store, logger: logger}
}

func (d *Drafts) Create(ctx context.Context, projectID string, form []byte, step, failure string) (*domain.ExperimentDraft, error) {
	draft, err := d.store.CreateExperimentDraft(ctx, projectID, form, step, failure)
	return draft, d.refusal("create experiment draft", err)
}

func (d *Drafts) List(ctx context.Context, projectID string) ([]*domain.ExperimentDraft, error) {
	drafts, err := d.store.ListExperimentDrafts(ctx, projectID)
	if err != nil {
		return nil, auth.Internal(d.logger, "list experiment drafts", err)
	}
	return drafts, nil
}

func (d *Drafts) Get(ctx context.Context, projectID, id string) (*domain.ExperimentDraft, error) {
	draft, err := d.store.GetExperimentDraft(ctx, projectID, id)
	return draft, d.refusal("read experiment draft", err)
}

func (d *Drafts) Update(ctx context.Context, projectID, id string, form []byte, step, failure string) (*domain.ExperimentDraft, error) {
	draft, err := d.store.UpdateExperimentDraft(ctx, projectID, id, form, step, failure)
	return draft, d.refusal("update experiment draft", err)
}

func (d *Drafts) Delete(ctx context.Context, projectID, id string) error {
	return d.refusal("delete experiment draft", d.store.DeleteExperimentDraft(ctx, projectID, id))
}

func (d *Drafts) refusal(what string, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, db.ErrExperimentDraftNotFound):
		return auth.NotFound("experiment draft")
	default:
		return auth.Internal(d.logger, what, err)
	}
}
