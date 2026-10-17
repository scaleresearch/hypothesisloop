package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/platformexperiments"
)

// sharedFlags is a SharingStore over a map of platform experiment id to whether it is shared.
type sharedFlags struct {
	shared map[string]bool
	fail   error
}

func (s *sharedFlags) SetPlatformExperimentShared(_ context.Context, id string, shared bool) (bool, error) {
	if s.fail != nil {
		return false, s.fail
	}
	if _, ok := s.shared[id]; !ok {
		return false, nil
	}
	s.shared[id] = shared
	return true, nil
}

func (s *sharedFlags) GetPlatformExperiment(_ context.Context, id string) (*domain.PlatformExperiment, error) {
	shared, ok := s.shared[id]
	if !ok {
		return nil, nil
	}
	return &domain.PlatformExperiment{ID: id, Shared: shared}, nil
}

func shareAs(t *testing.T, store platformexperiments.SharingStore, caller auth.Principal, path string) (int, map[string]any) {
	t.Helper()
	router := authtest.Router(authtest.As(caller))
	doc := NewDoc(router, "test", "1.0.0", "", auth.NewAccess(authtest.Placement{}), zap.NewNop())
	answer := func(_ context.Context, pe *domain.PlatformExperiment) (*struct{ Body *domain.PlatformExperiment }, error) {
		return &struct{ Body *domain.PlatformExperiment }{Body: pe}, nil
	}
	registerSharing(doc, platformexperiments.NewSharing(store, zap.NewNop()), answer)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, authtest.Authorize(httptest.NewRequest("POST", path, nil)))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("POST %s: body %q is not JSON: %v", path, rec.Body, err)
	}
	return rec.Code, body
}

func TestStaffShareAPlatformExperimentAndTakeItBack(t *testing.T) {
	store := &sharedFlags{shared: map[string]bool{"pe-1": false}}
	staff := auth.Principal{Kind: auth.KindStaff}
	for _, step := range []struct {
		path string
		want bool
	}{{"/platform-experiments/pe-1/share", true}, {"/platform-experiments/pe-1/share", true}, {"/platform-experiments/pe-1/unshare", false}} {
		code, body := shareAs(t, store, staff, step.path)
		if code != http.StatusOK || body["id"] != "pe-1" || body["shared"] != step.want || store.shared["pe-1"] != step.want {
			t.Fatalf("POST %s: got = %d %v, stored = %v, want the platform experiment with shared = %v", step.path, code, body, store.shared["pe-1"], step.want)
		}
	}
	for _, path := range []string{"/platform-experiments/pe-none/share", "/platform-experiments/pe-none/unshare", "/platform-experiments/pe-*/share"} {
		code, body := shareAs(t, store, staff, path)
		if code != http.StatusNotFound || body["reason"] != "not_found" || body["message"] == "" {
			t.Errorf("POST %s: got = %d %v, want 404 with reason not_found", path, code, body)
		}
	}
}

func TestOnlyStaffShareAPlatformExperiment(t *testing.T) {
	store := &sharedFlags{shared: map[string]bool{"pe-1": false}}
	for name, caller := range map[string]auth.Principal{
		"the owner of its org":  {Kind: auth.KindUser, UserID: "u-owner", OrgIDs: []string{"org-a"}},
		"an API key of its org": {Kind: auth.KindKey, KeyID: "k", OrgIDs: []string{"org-a"}},
		"its coordinator":       {Kind: auth.KindCoordinator, PlatformExperimentID: "pe-1", OrgIDs: []string{"org-a"}},
		"one of its agents":     {Kind: auth.KindAgent, AgentID: "agent-1", PlatformExperimentID: "pe-1", OrgIDs: []string{"org-a"}},
	} {
		for _, verb := range []string{"share", "unshare"} {
			if code, _ := shareAs(t, store, caller, "/platform-experiments/pe-1/"+verb); code != http.StatusForbidden {
				t.Errorf("%s as %s: got = %d, want = 403", verb, name, code)
			}
		}
	}
	if store.shared["pe-1"] {
		t.Fatal("a refused share was stored")
	}
}

func TestAFailedShareSaysNothingOfWhatFailed(t *testing.T) {
	store := &sharedFlags{shared: map[string]bool{"pe-1": false}, fail: errors.New("connection to 10.0.0.7 refused")}
	code, body := shareAs(t, store, auth.Principal{Kind: auth.KindStaff}, "/platform-experiments/pe-1/share")
	if code != http.StatusInternalServerError || body["error"] != auth.InternalError || len(body) != 1 {
		t.Fatalf("got = %d %v, want a 500 that carries no internal text", code, body)
	}
}
