package db

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestExperimentDraftsAreKeptPerProjectAsTyped(t *testing.T) {
	pool := eventsTestDB(t)
	accounts := NewStore(pool, 3)
	drafts := NewStore(pool, 3)
	ctx := context.Background()
	_, org := newTestUserHome(t, accounts)
	project := defaultProjectOf(t, accounts, org).ID
	other, err := accounts.CreateProject(ctx, org, "other", 0)
	if err != nil {
		t.Fatal(err)
	}

	form := []byte(`{"name":"half typed","budget_accelerator_hours":"","stages":null}`)
	created, err := drafts.CreateExperimentDraft(ctx, project, form, "2", "")
	if err != nil {
		t.Fatalf("create: got = %v, want = nil", err)
	}
	var stored map[string]any
	if err := json.Unmarshal(created.Form, &stored); err != nil || stored["name"] != "half typed" || created.Step != "2" || created.ProjectID != project {
		t.Fatalf("created: got = %+v, %v, want the form as sent on step 2", created, err)
	}
	if _, err := drafts.CreateExperimentDraft(ctx, "prj_missing", form, "1", ""); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("create in a missing project: got = %v, want = %v", err, ErrProjectNotFound)
	}

	updated, err := drafts.UpdateExperimentDraft(ctx, project, created.ID, []byte(`{"name":"finished"}`), "3", "repository creation failed")
	if err != nil || updated.Failure != "repository creation failed" || updated.Step != "3" || !updated.UpdatedAt.After(created.UpdatedAt) {
		t.Fatalf("update: got = %+v, %v, want the new form, step and failure", updated, err)
	}
	if _, err := drafts.UpdateExperimentDraft(ctx, other.ID, created.ID, form, "1", ""); !errors.Is(err, ErrExperimentDraftNotFound) {
		t.Fatalf("update through another project: got = %v, want = %v", err, ErrExperimentDraftNotFound)
	}
	if _, err := drafts.GetExperimentDraft(ctx, other.ID, created.ID); !errors.Is(err, ErrExperimentDraftNotFound) {
		t.Fatalf("read through another project: got = %v, want = %v", err, ErrExperimentDraftNotFound)
	}

	second, err := drafts.CreateExperimentDraft(ctx, project, []byte(`{}`), "1", "")
	if err != nil {
		t.Fatal(err)
	}
	listed, err := drafts.ListExperimentDrafts(ctx, project)
	if err != nil || len(listed) != 2 || listed[0].ID != second.ID || listed[1].ID != created.ID {
		t.Fatalf("list: got = %+v, %v, want the newest first", listed, err)
	}
	if none, err := drafts.ListExperimentDrafts(ctx, other.ID); err != nil || len(none) != 0 {
		t.Fatalf("list of another project: got = %+v, %v, want none", none, err)
	}

	if err := drafts.DeleteExperimentDraft(ctx, other.ID, created.ID); !errors.Is(err, ErrExperimentDraftNotFound) {
		t.Fatalf("delete through another project: got = %v, want = %v", err, ErrExperimentDraftNotFound)
	}
	if err := drafts.DeleteExperimentDraft(ctx, project, created.ID); err != nil {
		t.Fatalf("delete: got = %v, want = nil", err)
	}
	if _, err := drafts.GetExperimentDraft(ctx, project, created.ID); !errors.Is(err, ErrExperimentDraftNotFound) {
		t.Fatalf("read after delete: got = %v, want = %v", err, ErrExperimentDraftNotFound)
	}
}
