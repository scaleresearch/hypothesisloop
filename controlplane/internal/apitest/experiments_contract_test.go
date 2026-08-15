package apitest

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/scaleresearch/hypothesisloop/controlplane/services/scheduler"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// filterSpyStore records the filter list-experiments builds, so the test asserts on what the
// handler actually forwards to storage rather than on a re-implementation of the query.
type filterSpyStore struct {
	scheduler.Store
	gotList  domain.ExperimentFilter
	gotCount domain.ExperimentFilter
	items    []*domain.Experiment
	total    int
}

func (s *filterSpyStore) ListExperiments(_ context.Context, f domain.ExperimentFilter) ([]*domain.Experiment, error) {
	s.gotList = f
	return s.items, nil
}

func (s *filterSpyStore) CountExperiments(_ context.Context, f domain.ExperimentFilter) (int, error) {
	s.gotCount = f
	return s.total, nil
}

func schedulerRouter(store scheduler.Store) chi.Router {
	r := chi.NewRouter()
	d := apidocs.New(r, "scheduler", "1.0.0", "")
	scheduler.RegisterHuma(d, scheduler.NewHandler(scheduler.NewService(store, nil, nil, nil, nil)))
	return r
}

func TestListExperimentsForwardsEveryFilter(t *testing.T) {
	spy := &filterSpyStore{total: 45}
	r := schedulerRouter(spy)

	code, _ := get(t, r, "/experiments?agent=a1&platform_experiment_id=pe-1&status=RUNNING"+
		"&hypothesis_id=h-1&project_id=proj-1&since=2026-01-02T03:04:05Z"+
		"&search=min_lr&limit=10&offset=20&sort=-created_at")
	if code != 200 {
		t.Fatalf("status = %d, want 200", code)
	}

	want := domain.ExperimentFilter{
		AgentID: "a1", PlatformExperimentID: "pe-1", Status: domain.ExperimentStatus("RUNNING"),
		HypothesisID: "h-1", ProjectID: "proj-1",
		Since:  time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Search: "min_lr", Limit: 10, Offset: 20, Sort: "-created_at",
	}
	if spy.gotList != want {
		t.Errorf("ListExperiments filter =\n %+v\nwant\n %+v", spy.gotList, want)
	}
	// The total must describe the whole match set, so Count sees the same predicates. It
	// legitimately still carries Limit/Offset — the store ignores them (see CountExperiments).
	if spy.gotCount.PlatformExperimentID != want.PlatformExperimentID ||
		spy.gotCount.Search != want.Search || spy.gotCount.AgentID != want.AgentID ||
		spy.gotCount.Status != want.Status {
		t.Errorf("CountExperiments filter predicates = %+v, want them to match %+v", spy.gotCount, want)
	}
}

// The bug this endpoint was fixed for: an unrecognized filter silently returned every row.
func TestListExperimentsHonoursPlatformExperimentFilter(t *testing.T) {
	spy := &filterSpyStore{}
	r := schedulerRouter(spy)

	if code, _ := get(t, r, "/experiments?platform_experiment_id=pe-d564bb90"); code != 200 {
		t.Fatalf("status = %d, want 200", code)
	}
	if spy.gotList.PlatformExperimentID != "pe-d564bb90" {
		t.Fatalf("platform_experiment_id was dropped: filter = %+v", spy.gotList)
	}
}

func TestListExperimentsReportsTotalCountHeader(t *testing.T) {
	spy := &filterSpyStore{
		total: 45,
		items: []*domain.Experiment{{ID: "job-1"}, {ID: "job-2"}},
	}
	r := schedulerRouter(spy)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/experiments?limit=2", nil))
	if got := rec.Header().Get("X-Total-Count"); got != "45" {
		t.Errorf("X-Total-Count = %q, want %q — a page must report the full match count", got, "45")
	}

	var body []*domain.Experiment
	b, _ := io.ReadAll(rec.Body)
	if err := json.Unmarshal(b, &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if len(body) != 2 {
		t.Errorf("returned %d items, want the 2 on this page", len(body))
	}
}

func TestListExperimentsDefaultsToNoFilter(t *testing.T) {
	spy := &filterSpyStore{}
	r := schedulerRouter(spy)

	if code, _ := get(t, r, "/experiments"); code != 200 {
		t.Fatalf("status = %d, want 200", code)
	}
	if (spy.gotList != domain.ExperimentFilter{}) {
		t.Errorf("bare list built filter %+v, want the zero filter", spy.gotList)
	}
}

// The original defect was an unrecognized filter being ignored: the caller believed it had
// filtered and read the whole table instead. Unknown params must be refused, not dropped.
func TestUnknownQueryParameterIsRejected(t *testing.T) {
	spy := &filterSpyStore{}
	r := schedulerRouter(spy)

	code, body := get(t, r, "/experiments?platfrom_experiment_id=pe-1")
	if code != http.StatusUnprocessableEntity && code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 4xx for an unknown query param; body=%s", code, body)
	}
	if !strings.Contains(body, "platfrom_experiment_id") {
		t.Errorf("error should name the offending param, got: %s", body)
	}
}

func TestKnownQueryParametersStillAccepted(t *testing.T) {
	spy := &filterSpyStore{}
	r := schedulerRouter(spy)

	for _, q := range []string{
		"/experiments",
		"/experiments?agent=a1",
		"/experiments?platform_experiment_id=pe-1&status=RUNNING&search=x&limit=1&offset=0&sort=-created_at",
	} {
		if code, body := get(t, r, q); code != 200 {
			t.Errorf("GET %s = %d, want 200; body=%s", q, code, body)
		}
	}
}

// status is a Postgres enum, so an unrecognized value used to reach the driver and surface as a
// 500 — a client mistake reported as a server fault.
func TestUnknownStatusIsClientError(t *testing.T) {
	r := schedulerRouter(&filterSpyStore{})
	if code, body := get(t, r, "/experiments?status=bogus"); code != http.StatusBadRequest {
		t.Fatalf("status=bogus returned %d, want 400; body=%s", code, body)
	}
	for _, s := range []string{"QUEUED", "RUNNING", "COMPLETED", "EVICTED", "REJECTED", "FAILED", "SUBMITTED", "ADMITTED"} {
		if code, body := get(t, r, "/experiments?status="+s); code != 200 {
			t.Errorf("status=%s returned %d, want 200; body=%s", s, code, body)
		}
	}
}

// A mistyped sort field used to fall through to the default order, so the caller got a
// differently-ordered page with no sign the sort had been dropped.
func TestInvalidSortAndPaginationAreRejected(t *testing.T) {
	r := schedulerRouter(&filterSpyStore{})
	for _, q := range []string{
		"/experiments?sort=-created", // near-miss for created_at
		"/experiments?sort=bogus",
		"/experiments?since=yesterday", // must be RFC3339, not prose
		"/experiments?limit=-1",
		"/experiments?offset=-1",
	} {
		if code, body := get(t, r, q); code != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400; body=%s", q, code, body)
		}
	}
	for _, q := range []string{
		"/experiments?sort=created_at", "/experiments?sort=-created_at",
		"/experiments?sort=priority_score", "/experiments?sort=-status",
		"/experiments?limit=0&offset=0",
	} {
		if code, body := get(t, r, q); code != 200 {
			t.Errorf("GET %s = %d, want 200; body=%s", q, code, body)
		}
	}
}
