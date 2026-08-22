package registry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/objectstore"
)

// emptyBucketStore answers every listing the way a real store answers a prefix nothing was ever
// written to: a successful, empty result.
type emptyBucketStore struct {
	Store
	exp *domain.Experiment
}

func (s emptyBucketStore) GetExperiment(_ context.Context, _ string) (*domain.Experiment, error) {
	return s.exp, nil
}

// Most jobs keep nothing — their whole result is a metric stream. If "this job saved no files"
// came back as an error, every caller would have to tell that apart from a store outage, and the
// UI would show a failure for the ordinary case. It is an empty list, and it is never nil: a JSON
// null here would make callers handle a third state that means the same as the second.
func TestListDataReturnsEmptyForAJobThatWroteNothing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>`))
	}))
	defer srv.Close()
	client, err := objectstore.New(srv.URL, "us-east-1", "hl-data", "key", "secret")
	if err != nil {
		t.Fatal(err)
	}
	svc := &Service{
		store:     emptyBucketStore{exp: &domain.Experiment{ID: "quiet-job", AgentID: "agent", PlatformExperimentID: "pe-1"}},
		dataStore: client,
	}

	objects, err := svc.ListData(context.Background(), "quiet-job")
	if err != nil {
		t.Fatalf("ListData: a job that wrote nothing must list as empty, not fail: %v", err)
	}
	if objects == nil {
		t.Fatal("ListData returned a nil slice; callers must never have to distinguish nil from empty")
	}
	if len(objects) != 0 {
		t.Fatalf("ListData returned %d objects, want 0", len(objects))
	}
}
