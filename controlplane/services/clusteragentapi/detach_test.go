package clusteragentapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
)

type detachStore struct {
	*reconcileStore
	removed []string
	err     error
}

func (s *detachStore) RemoveCluster(_ context.Context, name string) ([]string, error) {
	if s.err != nil {
		return nil, s.err
	}
	s.removed = append(s.removed, name)
	return []string{"job-1", "job-2"}, nil
}

func detach(t *testing.T, w *reconcileWorld, cluster string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	w.router.ServeHTTP(rec, authtest.Authorize(httptest.NewRequest("DELETE", "/"+cluster, nil)))
	return rec
}

// A machine leaves by itself: its runtime ends its own cluster and is told which jobs went back
// to the queue. The token of one cluster ends no other.
func TestARuntimeDetachesItsOwnClusterAndNoOther(t *testing.T) {
	store := &detachStore{}
	w := newReconcileWorldWith(t, "of-a", func(base *reconcileStore) Store {
		store.reconcileStore = base
		return store
	})

	rec := detach(t, w, "of-b")
	if rec.Code != http.StatusForbidden || len(store.removed) != 0 {
		t.Fatalf("detaching another cluster: status = %d, removed = %v, want 403 and nothing removed", rec.Code, store.removed)
	}

	rec = detach(t, w, "of-a")
	if rec.Code != http.StatusOK {
		t.Fatalf("detach: status = %d, body = %s", rec.Code, rec.Body)
	}
	var body struct {
		Requeued []string `json:"requeued"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(store.removed) != 1 || store.removed[0] != "of-a" || len(body.Requeued) != 2 {
		t.Fatalf("removed = %v, requeued = %v, want of-a removed and its two jobs named", store.removed, body.Requeued)
	}
}

func TestDetachingAClusterThatIsGoneIsNotFound(t *testing.T) {
	store := &detachStore{err: db.ErrClusterNotFound}
	w := newReconcileWorldWith(t, "of-a", func(base *reconcileStore) Store {
		store.reconcileStore = base
		return store
	})
	if rec := detach(t, w, "of-a"); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
