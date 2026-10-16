package clusteragentapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
)

func (s *reconcileStore) RecordContactRefusal(_ context.Context, name, refusal string) error {
	s.refusals = append(s.refusals, name+"="+refusal)
	return nil
}

// A cluster whose reconcile is refused keeps the refusal as its last error, which its owner reads
// beside its last contact, and the next accepted reconcile clears it.
func TestARefusedReconcileIsKeptAsTheClustersLastErrorUntilOneIsAccepted(t *testing.T) {
	w := newReconcileWorld(t, "of-a")
	w.store.bindErr = db.ErrClusterIDChanged
	if rec := w.reconcile(t, "of-a", idleReport("id-rebuilt")); rec.Code != http.StatusConflict {
		t.Fatalf("reconcile under another identity: got = %d %s, want = 409", rec.Code, rec.Body)
	}
	if len(w.store.refusals) != 1 || !strings.HasPrefix(w.store.refusals[0], "of-a=") || !strings.Contains(w.store.refusals[0], "forget-identity") {
		t.Fatalf("refusals = %q, want the conflict kept in the platform's words", w.store.refusals)
	}
	w.store.bindErr = nil
	if rec := w.reconcile(t, "of-a", idleReport("id-of-a")); rec.Code != http.StatusOK {
		t.Fatalf("reconcile: got = %d %s, want = 200", rec.Code, rec.Body)
	}
	if len(w.store.refusals) != 2 || w.store.refusals[1] != "of-a=" {
		t.Fatalf("refusals = %q, want the accepted reconcile to clear the last error", w.store.refusals)
	}
}

// The agent's version comes on every reconcile; one no image tag could be is refused before
// anything is recorded.
func TestAnAgentVersionNoTagCouldBeIsRefused(t *testing.T) {
	w := newReconcileWorld(t, "of-a")
	for _, version := range []string{strings.Repeat("v", maxAgentVersionLength+1), "abc\n1234"} {
		body, err := json.Marshal(idleReport("id-of-a"))
		if err != nil {
			t.Fatal(err)
		}
		req := authtest.Authorize(httptest.NewRequest("POST", "/of-a/reconcile", bytes.NewReader(body)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(clusterapi.AgentVersionHeader, version)
		rec := httptest.NewRecorder()
		w.router.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest || w.metricsWrites.Load() != 0 {
			t.Errorf("version %q: got = %d %s with %d metric writes, want 400 and nothing recorded", version, rec.Code, rec.Body, w.metricsWrites.Load())
		}
	}
}

// An installer's dry run asks whether the token is taken, and nothing is recorded for it: no
// capacity, no identity and no contact.
func TestTheAccessCheckAnswersTheTokenAndRecordsNothing(t *testing.T) {
	w := newReconcileWorld(t, "of-a")
	rec := httptest.NewRecorder()
	w.router.ServeHTTP(rec, authtest.Authorize(httptest.NewRequest("GET", "/of-a/access", nil)))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("access: got = %d %s, want = 204", rec.Code, rec.Body)
	}
	if w.metricsWrites.Load() != 0 || len(w.store.binds) != 0 || len(w.store.refusals) != 0 {
		t.Fatalf("the access check recorded %d metric writes, binds %v and refusals %v, want nothing", w.metricsWrites.Load(), w.store.binds, w.store.refusals)
	}
}
