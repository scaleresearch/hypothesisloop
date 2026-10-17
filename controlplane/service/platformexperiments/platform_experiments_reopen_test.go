package platformexperiments

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
)

// reopenStore reopens as the database does, and records what it was asked to write.
type reopenStore struct {
	statusStore
	continued map[string]bool
	reopened  []string
	endsAt    time.Time
	duration  float64
	model     string
}

func (s *reopenStore) ReopenPlatformExperiment(_ context.Context, id string, endsAt time.Time, durationHours float64, coordinatorModel string) error {
	for _, pe := range s.created {
		if pe.ID != id {
			continue
		}
		if pe.Status != domain.PlatformExpClosed || pe.StartedAt.IsZero() || s.continued[id] || coordinatorModel != "" && !pe.CoordinatorHosted {
			return db.ErrNotReopenable
		}
		pe.Status, pe.ClosedAt, pe.ClosedBy, pe.CloseReason, pe.CloseMessage = domain.PlatformExpRunning, nil, nil, "", ""
		pe.EndsAt, pe.DurationHours = endsAt, durationHours
		if coordinatorModel != "" {
			pe.Coordinator.Model = coordinatorModel
		}
		s.reopened, s.endsAt, s.duration, s.model = append(s.reopened, id), endsAt, durationHours, coordinatorModel
		return nil
	}
	return db.ErrPlatformExperimentNotFound
}

func reopenRouter(t *testing.T, store *reopenStore, caller auth.Principal) http.Handler {
	t.Helper()
	tenancy := map[string]domain.Tenancy{}
	for _, pe := range store.created {
		tenancy[pe.ID] = domain.Tenancy{PlatformExperimentID: pe.ID, ProjectID: "prj-a", OrgID: "org-a"}
	}
	r := authtest.Router(authtest.As(caller))
	doc := apidocs.New(r, "test", "1.0.0", "", auth.NewAccess(authtest.Placement{PlatformExperiments: tenancy}), zap.NewNop())
	handler := NewPlatformExperimentsHandler(newSignupTestService(t, store), &fakeRepositories{}, testPlanning, zap.NewNop()).
		WithAgentLaunch(AgentLaunch{Hosting: true, DefaultModel: "model-default", CoordinatorModel: "coordinator-default", SigningSecret: "a-secret-only-this-test-knows-00", MaxHosted: hostingMaxHosted})
	registerHuma(doc, NewHandler(nil, zap.NewNop()), handler)
	return r
}

func closedRun(id string, startedAt time.Time, hosted bool) *domain.PlatformExperiment {
	closedAt := startedAt.Add(time.Hour)
	pe := &domain.PlatformExperiment{
		ID: id, ProjectID: "prj-a", Status: domain.PlatformExpClosed, Stages: []domain.Stage{{LengthPct: 100}},
		StartsAt: startedAt, StartedAt: startedAt, DurationHours: 24, EndsAt: startedAt.Add(24 * time.Hour),
		ClosedAt: &closedAt, CloseReason: domain.CloseByRequest, CloseMessage: "redeploying the platform", ClosedBy: &domain.Actor{Kind: domain.ActorStaff},
	}
	if hosted {
		pe.CoordinatorHosted, pe.Coordinator = true, &domain.HostedCoordinator{Model: "coordinator-old"}
	}
	return pe
}

func TestStaffReopenAClosedRunUntilANewEndAndItReadsRunning(t *testing.T) {
	start := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	store := &reopenStore{statusStore: statusStore{createdStore: createdStore{created: []*domain.PlatformExperiment{closedRun("pe-1", start, true)}}}}
	r := reopenRouter(t, store, auth.Principal{Kind: auth.KindStaff})
	end := start.Add(30 * time.Hour)

	code, out := call(t, r, "POST", "/platform-experiments/pe-1/reopen", fmt.Sprintf(`{"ends_at":%q}`, end.Format(time.RFC3339)))
	if code != http.StatusOK || out["status"] != "running" || out["closed_at"] != nil || out["close_reason"] != nil || out["closed_by"] != nil {
		t.Fatalf("reopen: got = %d %v, want 200 and the run as running with nothing of the close", code, out)
	}
	if !store.endsAt.Equal(end) || store.duration != 30 || store.model != "" {
		t.Fatalf("stored ends_at %s, duration_hours %g, coordinator_model %q: want %s, 30 counted from the start and the model kept", store.endsAt, store.duration, store.model, end)
	}
	if code, out := call(t, r, "POST", "/platform-experiments/pe-1/reopen", fmt.Sprintf(`{"ends_at":%q}`, end.Format(time.RFC3339))); code != http.StatusConflict || out["reason"] != "invalid_transition" {
		t.Fatalf("a second reopen: got = %d %v, want 409 invalid_transition", code, out)
	}
}

func TestAReopenMovesAHostedCoordinatorToTheModelItNamesCheckedAsACreateIs(t *testing.T) {
	start := time.Now().UTC().Add(-2 * time.Hour)
	store := &reopenStore{statusStore: statusStore{createdStore: createdStore{created: []*domain.PlatformExperiment{closedRun("pe-1", start, true)}}}}
	r := reopenRouter(t, store, auth.Principal{Kind: auth.KindStaff})
	end := start.Add(10 * time.Hour).Format(time.RFC3339)

	if code, out := call(t, r, "POST", "/platform-experiments/pe-1/reopen", fmt.Sprintf(`{"ends_at":%q,"coordinator_model":"no spaces allowed"}`, end)); code != http.StatusBadRequest || out["reason"] != "invalid_request" || len(store.reopened) != 0 {
		t.Fatalf("an invalid model: got = %d %v, want 400 invalid_request and nothing written", code, out)
	}
	if code, out := call(t, r, "POST", "/platform-experiments/pe-1/reopen", fmt.Sprintf(`{"ends_at":%q,"coordinator_model":"coordinator-new-2"}`, end)); code != http.StatusOK || store.model != "coordinator-new-2" {
		t.Fatalf("a new model: got = %d %v, stored %q, want 200 and the model written with the reopen", code, out, store.model)
	}
}

func TestAReopenIsRefusedWithAPlainReason(t *testing.T) {
	now := time.Now().UTC()
	neverStarted := closedRun("pe-never", now.Add(-2*time.Hour), false)
	neverStarted.StartedAt, neverStarted.CloseReason = time.Time{}, domain.CloseAgentsNotStarted
	running := closedRun("pe-running", now.Add(-2*time.Hour), false)
	running.Status, running.ClosedAt = domain.PlatformExpRunning, nil
	store := &reopenStore{
		statusStore: statusStore{createdStore: createdStore{created: []*domain.PlatformExperiment{
			closedRun("pe-1", now.Add(-2*time.Hour), false), neverStarted, running, closedRun("pe-continued", now.Add(-2*time.Hour), false),
		}}},
		continued: map[string]bool{"pe-continued": true},
	}
	staff := reopenRouter(t, store, auth.Principal{Kind: auth.KindStaff})
	ahead := now.Add(5 * time.Hour).Format(time.RFC3339)
	for name, c := range map[string]struct {
		path, body, reason string
		status             int
	}{
		"one still running":                  {"/platform-experiments/pe-running/reopen", fmt.Sprintf(`{"ends_at":%q}`, ahead), "invalid_transition", 409},
		"one closed before its run started":  {"/platform-experiments/pe-never/reopen", fmt.Sprintf(`{"ends_at":%q}`, ahead), "invalid_transition", 409},
		"one a later run continues":          {"/platform-experiments/pe-continued/reopen", fmt.Sprintf(`{"ends_at":%q}`, ahead), "invalid_transition", 409},
		"an ends_at that has passed":         {"/platform-experiments/pe-1/reopen", fmt.Sprintf(`{"ends_at":%q}`, now.Add(-time.Minute).Format(time.RFC3339)), "invalid_length", 422},
		"an ends_at past the longest run":    {"/platform-experiments/pe-1/reopen", fmt.Sprintf(`{"ends_at":%q}`, now.Add(testSettings.MaxRunLength).Format(time.RFC3339)), "run_too_long", 422},
		"a model with no hosted coordinator": {"/platform-experiments/pe-1/reopen", fmt.Sprintf(`{"ends_at":%q,"coordinator_model":"coordinator-new"}`, ahead), "invalid_request", 400},
		"one that does not exist":            {"/platform-experiments/pe-none/reopen", fmt.Sprintf(`{"ends_at":%q}`, ahead), "not_found", 404},
	} {
		if code, out := call(t, staff, "POST", c.path, c.body); code != c.status || out["reason"] != c.reason || out["message"] == "" {
			t.Errorf("%s: got = %d %v, want %d %s with a message", name, code, out, c.status, c.reason)
		}
	}
	for name, caller := range map[string]auth.Principal{
		"the owner of its org":  {Kind: auth.KindUser, UserID: "u-owner", OrgIDs: []string{"org-a"}},
		"an API key of its org": {Kind: auth.KindKey, KeyID: "k", OrgIDs: []string{"org-a"}},
		"its coordinator":       {Kind: auth.KindCoordinator, PlatformExperimentID: "pe-1", OrgIDs: []string{"org-a"}},
	} {
		if code, _ := call(t, reopenRouter(t, store, caller), "POST", "/platform-experiments/pe-1/reopen", fmt.Sprintf(`{"ends_at":%q}`, ahead)); code != http.StatusForbidden {
			t.Errorf("a reopen by %s: got = %d, want 403", name, code)
		}
	}
	if len(store.reopened) != 0 {
		t.Fatalf("reopened %v, want nothing written by a refused reopen", store.reopened)
	}
}
