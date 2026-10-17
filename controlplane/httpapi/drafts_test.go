package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/platformexperiments"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
)

type memoryDrafts struct {
	byID map[string]*domain.ExperimentDraft
	next int
}

func (m *memoryDrafts) CreateExperimentDraft(_ context.Context, projectID string, form []byte, step, failure string) (*domain.ExperimentDraft, error) {
	m.next++
	d := &domain.ExperimentDraft{ID: "draft-" + string(rune('0'+m.next)), ProjectID: projectID, Form: append([]byte(nil), form...), Step: step, Failure: failure, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	m.byID[d.ID] = d
	return d, nil
}

func (m *memoryDrafts) ListExperimentDrafts(_ context.Context, projectID string) ([]*domain.ExperimentDraft, error) {
	out := []*domain.ExperimentDraft{}
	for _, d := range m.byID {
		if d.ProjectID == projectID {
			out = append(out, d)
		}
	}
	return out, nil
}

func (m *memoryDrafts) GetExperimentDraft(_ context.Context, projectID, id string) (*domain.ExperimentDraft, error) {
	d, ok := m.byID[id]
	if !ok || d.ProjectID != projectID {
		return nil, db.ErrExperimentDraftNotFound
	}
	return d, nil
}

func (m *memoryDrafts) UpdateExperimentDraft(ctx context.Context, projectID, id string, form []byte, step, failure string) (*domain.ExperimentDraft, error) {
	d, err := m.GetExperimentDraft(ctx, projectID, id)
	if err != nil {
		return nil, err
	}
	d.Form, d.Step, d.Failure = append([]byte(nil), form...), step, failure
	return d, nil
}

func (m *memoryDrafts) DeleteExperimentDraft(ctx context.Context, projectID, id string) error {
	if _, err := m.GetExperimentDraft(ctx, projectID, id); err != nil {
		return err
	}
	delete(m.byID, id)
	return nil
}

var draftPlacement = authtest.Placement{Projects: map[string]domain.Project{
	"prj-a": {ID: "prj-a", OrgID: "org-a"},
	"prj-b": {ID: "prj-b", OrgID: "org-b"},
}}

func draftCall(t *testing.T, store platformexperiments.ExperimentDraftStore, caller auth.Principal, method, path, body string) (int, string) {
	t.Helper()
	r := authtest.Router(authtest.As(caller))
	doc := NewDoc(r, "test", "1.0.0", "", auth.NewAccess(draftPlacement), zap.NewNop())
	registerExperimentDrafts(doc, platformexperiments.NewDrafts(store, zap.NewNop()))
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, authtest.Authorize(req))
	return rec.Code, rec.Body.String()
}

func TestADraftIsKeptAsTypedAndFinishedOrDeletedWithinItsProject(t *testing.T) {
	store := &memoryDrafts{byID: map[string]*domain.ExperimentDraft{}}
	owner := auth.Principal{Kind: auth.KindUser, UserID: "u-1", OrgIDs: []string{"org-a"}}

	code, body := draftCall(t, store, owner, "POST", "/projects/prj-a/experiment-drafts", `{"form":{"name":"","budget_accelerator_hours":"x"},"step":"2"}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s, want 201", code, body)
	}
	var created domain.ExperimentDraft
	if err := json.Unmarshal([]byte(body), &created); err != nil || created.Step != "2" || string(created.Form) != `{"name":"","budget_accelerator_hours":"x"}` {
		t.Fatalf("created: %s, %v, want the form unread on step 2", body, err)
	}
	draft := "/projects/prj-a/experiment-drafts/" + created.ID

	if code, body := draftCall(t, store, owner, "PUT", draft, `{"form":{"name":"n"},"step":"3","failure":"repository creation failed"}`); code != http.StatusOK || !strings.Contains(body, `"failure":"repository creation failed"`) {
		t.Fatalf("put: %d %s, want the failure kept", code, body)
	}
	if code, body := draftCall(t, store, owner, "GET", "/projects/prj-a/experiment-drafts", ""); code != http.StatusOK || !strings.Contains(body, created.ID) {
		t.Fatalf("list: %d %s", code, body)
	}
	for _, form := range []string{`"text"`, `null`, `[1]`} {
		if code, body := draftCall(t, store, owner, "PUT", draft, `{"form":`+form+`,"step":"1"}`); code != http.StatusBadRequest {
			t.Fatalf("put a form of %s: %d %s, want 400", form, code, body)
		}
	}
	if code, _ := draftCall(t, store, owner, "DELETE", draft, ""); code != http.StatusNoContent {
		t.Fatalf("delete: %d, want 204", code)
	}
	if code, _ := draftCall(t, store, owner, "GET", draft, ""); code != http.StatusNotFound {
		t.Fatalf("read after delete: %d, want 404", code)
	}
}

func TestADraftOfAnotherOrgOrProjectIsNotFound(t *testing.T) {
	store := &memoryDrafts{byID: map[string]*domain.ExperimentDraft{}}
	d, _ := store.CreateExperimentDraft(context.Background(), "prj-a", []byte(`{}`), "1", "")
	draft := "/projects/prj-a/experiment-drafts/" + d.ID
	for name, caller := range map[string]auth.Principal{
		"a person of another org":  {Kind: auth.KindUser, UserID: "u-2", OrgIDs: []string{"org-b"}},
		"a key of another project": {Kind: auth.KindKey, OrgIDs: []string{"org-a"}, ProjectID: "prj-c"},
	} {
		if code, body := draftCall(t, store, caller, "GET", draft, ""); code != http.StatusNotFound {
			t.Errorf("%s reads it: %d %s, want 404", name, code, body)
		}
	}
	owner := auth.Principal{Kind: auth.KindUser, UserID: "u-3", OrgIDs: []string{"org-b"}}
	if code, body := draftCall(t, store, owner, "GET", "/projects/prj-b/experiment-drafts/"+d.ID, ""); code != http.StatusNotFound {
		t.Errorf("read through a project of the caller's own that does not hold it: %d %s, want 404", code, body)
	}
}
