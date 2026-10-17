package apitest

import (
	"reflect"

	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/services/scheduler"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
)

// filterSpyStore records the filter list-experiments builds, so the test asserts on what the
// handler actually forwards to storage rather than on a re-implementation of the query.
type filterSpyStore struct {
	scheduler.Store
	gotList    domain.ExperimentFilter
	gotCount   domain.ExperimentFilter
	gotStats   domain.ExperimentFilter
	gotGroupBy string
	items      []*domain.Experiment
	total      int
}

func (s *filterSpyStore) NameActors(_ context.Context, actors []*domain.Actor) error {
	for _, a := range actors {
		a.Name = a.ID
	}
	return nil
}

func (s *filterSpyStore) DescribeJobs(context.Context, []*domain.Experiment) error { return nil }

func (s *filterSpyStore) GetPlatformExperiment(_ context.Context, id string) (*domain.PlatformExperiment, error) {
	return &domain.PlatformExperiment{ID: id}, nil
}

func (s *filterSpyStore) ListExperiments(_ context.Context, f domain.ExperimentFilter) ([]*domain.Experiment, error) {
	s.gotList = f
	return s.items, nil
}

func (s *filterSpyStore) GetExperiment(_ context.Context, id string) (*domain.Experiment, error) {
	for _, e := range s.items {
		if e.ID == id {
			return e, nil
		}
	}
	return nil, nil
}

func (s *filterSpyStore) FindingSummary(context.Context, string) (string, error) { return "", nil }

func (s *filterSpyStore) CountExperiments(_ context.Context, f domain.ExperimentFilter) (int, error) {
	s.gotCount = f
	return s.total, nil
}

func (s *filterSpyStore) ExperimentStats(_ context.Context, f domain.ExperimentFilter, groupBy string) (*domain.ExperimentStats, error) {
	s.gotStats = f
	s.gotGroupBy = groupBy
	return &domain.ExperimentStats{Total: s.total, ByStatus: map[string]int{"RUNNING": s.total}}, nil
}

func schedulerRouter(t *testing.T, store scheduler.Store) chi.Router {
	t.Helper()
	// Phase detail and where a job ran are merged into every list/read from the metrics store,
	// so these contract tests need one that answers: no rows to SQL, no series to PromQL.
	metrics := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/sql" {
			_, _ = w.Write([]byte(`{"output":[{"records":{"schema":{"column_schemas":[{"name":"greptime_timestamp"},{"name":"greptime_value"}]},"rows":[]}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	t.Cleanup(metrics.Close)
	r := authtest.Router(authtest.Staff)
	d := apidocs.New(r, "scheduler", "1.0.0", "", auth.NewAccess(authtest.Placement{}), zap.NewNop())
	scheduler.RegisterHuma(d, scheduler.NewHandler(scheduler.NewService(store, nil, nil, worldRepositories{}, contractCapacity{}, contractSettler{}, metrics.URL, zap.NewNop()).WithEnvironments(noEnvironments{}).WithLoop(noopLoop{})))
	return r
}

func TestListExperimentsForwardsEveryFilter(t *testing.T) {
	spy := &filterSpyStore{total: 45}
	r := schedulerRouter(t, spy)

	code, _ := get(t, r, "/experiments?agent_id=a1&platform_experiment_id=pe-1&status=RUNNING"+
		"&hypothesis_id=h-1&project_id=proj-1&since=2026-01-02T03:04:05Z"+
		"&q=min_lr&limit=10&offset=20&sort=-created_at")
	if code != 200 {
		t.Fatalf("status = %d, want 200", code)
	}

	want := domain.ExperimentFilter{
		Scope:   domain.TenantScope{AllOrgs: true, ProjectID: "proj-1"},
		AgentID: "a1", PlatformExperimentID: "pe-1", Status: domain.ExperimentStatus("RUNNING"),
		HypothesisID: "h-1",
		Since:        time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Search:       "min_lr", Limit: 10, Offset: 20, Sort: "-created_at",
	}
	if !reflect.DeepEqual(spy.gotList, want) {
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

func TestListExperimentsReportsTotalCountHeader(t *testing.T) {
	spy := &filterSpyStore{
		total: 45,
		items: []*domain.Experiment{{ID: "job-1"}, {ID: "job-2"}},
	}
	r := schedulerRouter(t, spy)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, authtest.Authorize(httptest.NewRequest("GET", "/experiments?limit=2", nil)))
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

// A bare list must filter nothing but still ask for a small page. The caller is an agent reading
// the whole response into a bounded context window: an omitted ?limit reaching the store as "no
// limit" hands it every row ever submitted, and defaulting to the ceiling hands it a page an
// order of magnitude larger than it asked for. It is reading its latest jobs, so newest first.
func TestListExperimentsDefaultsToOneSmallPageNewestFirst(t *testing.T) {
	spy := &filterSpyStore{}
	r := schedulerRouter(t, spy)

	if code, _ := get(t, r, "/experiments"); code != 200 {
		t.Fatalf("status = %d, want 200", code)
	}
	want := domain.ExperimentFilter{Scope: domain.AllTenants, Limit: 20, Sort: "-created_at"}
	if !reflect.DeepEqual(spy.gotList, want) {
		t.Errorf("bare list built filter %+v, want %+v", spy.gotList, want)
	}
}

// A ?limit over the ceiling was clamped to it and answered 200, so a caller that asked for 200
// read 50 as the whole list. It is refused, as the other lists refuse it, and nothing is read.
func TestListExperimentsRefusesAPageItDoesNotServe(t *testing.T) {
	for _, query := range []string{"limit=51", "limit=100000", "limit=-1", "offset=-1"} {
		spy := &filterSpyStore{}
		r := schedulerRouter(t, spy)
		code, body := get(t, r, "/experiments?"+query)
		if code != http.StatusUnprocessableEntity || !strings.Contains(string(body), `"reason":"invalid_page"`) || !strings.Contains(string(body), "limit must be 1 to 50") {
			t.Errorf("?%s: status = %d, body = %s, want 422 invalid_page naming the ceiling", query, code, body)
		}
		if !reflect.DeepEqual(spy.gotList, domain.ExperimentFilter{}) {
			t.Errorf("?%s: the store was read with %+v, want no read", query, spy.gotList)
		}
	}
	spy := &filterSpyStore{}
	if code, _ := get(t, schedulerRouter(t, spy), "/experiments?limit=50"); code != 200 || spy.gotList.Limit != 50 {
		t.Errorf("?limit=50: status = %d, limit = %d, want the ceiling itself served", code, spy.gotList.Limit)
	}
}

// The original defect was an unrecognized filter being ignored: the caller believed it had
// filtered and read the whole table instead. Unknown params must be refused, not dropped.
func TestUnknownQueryParameterIsRejected(t *testing.T) {
	spy := &filterSpyStore{}
	r := schedulerRouter(t, spy)

	code, body := get(t, r, "/experiments?platfrom_experiment_id=pe-1")
	if code != http.StatusUnprocessableEntity && code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 4xx for an unknown query param; body=%s", code, body)
	}
	if !strings.Contains(body, "platfrom_experiment_id") {
		t.Errorf("error should name the offending param, got: %s", body)
	}
}

// status is a Postgres enum, so an unrecognized value used to reach the driver and surface as a
// 500 — a client mistake reported as a server fault.
func TestUnknownStatusIsClientError(t *testing.T) {
	r := schedulerRouter(t, &filterSpyStore{})
	if code, body := get(t, r, "/experiments?status=bogus"); code != http.StatusBadRequest {
		t.Fatalf("status=bogus returned %d, want 400; body=%s", code, body)
	}
	for _, s := range []string{"QUEUED", "RUNNING", "COMPLETED", "EVICTED", "REJECTED", "FAILED", "SUBMITTED"} {
		if code, body := get(t, r, "/experiments?status="+s); code != 200 {
			t.Errorf("status=%s returned %d, want 200; body=%s", s, code, body)
		}
	}
}

// A mistyped sort field used to fall through to the default order, so the caller got a
// differently-ordered page with no sign the sort had been dropped.
func TestInvalidSortAndPaginationAreRejected(t *testing.T) {
	r := schedulerRouter(t, &filterSpyStore{})
	for _, q := range []string{
		"/experiments?sort=-created", // near-miss for created_at
		"/experiments?sort=bogus",
		"/experiments?since=yesterday", // must be RFC3339, not prose
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

type contractCapacity struct{}

func (contractCapacity) GetTotalCapacity(context.Context) (map[string]domain.Footprint, error) {
	return nil, nil
}
func (contractCapacity) GetAutoscalerCapability(context.Context) (map[string]bool, error) {
	return nil, nil
}
func (contractCapacity) GetNodeTotalCapacity(context.Context) (map[string]map[string]map[string]int64, error) {
	return nil, nil
}
func (contractCapacity) GetMultiNodeCapability(context.Context) (map[string]bool, error) {
	return nil, nil
}

type contractSettler struct{}

func (contractSettler) Settle(context.Context, *domain.Experiment) error { return nil }

type noopLoop struct{}

func (noopLoop) Trigger() {}

// /experiments/stats must reach the stats handler, not be swallowed by /experiments/{id} as an
// experiment whose ID happens to be "stats" — a route-ordering slip that would 404 or, worse,
// answer with an unrelated body.
func TestExperimentStatsIsNotShadowedByExperimentByID(t *testing.T) {
	spy := &filterSpyStore{total: 7}
	r := schedulerRouter(t, spy)

	code, body := get(t, r, "/experiments/stats?status=RUNNING")
	if code != 200 {
		t.Fatalf("status = %d (%s), want 200", code, body)
	}
	var stats domain.ExperimentStats
	if err := json.Unmarshal([]byte(body), &stats); err != nil {
		t.Fatalf("unmarshal body %s: %v", body, err)
	}
	if stats.Total != 7 {
		t.Errorf("total = %d, want 7", stats.Total)
	}
	if spy.gotStats.Status != domain.StatusRunning {
		t.Errorf("stats filter status = %q, want RUNNING", spy.gotStats.Status)
	}
}

func TestExperimentStatsForwardsKnownGroupByAndRejectsOthers(t *testing.T) {
	spy := &filterSpyStore{}
	r := schedulerRouter(t, spy)

	if code, body := get(t, r, "/experiments/stats?platform_experiment_id=pe-1&group_by=agent"); code != 200 {
		t.Fatalf("status = %d (%s), want 200", code, body)
	}
	if spy.gotGroupBy != "agent" || spy.gotStats.PlatformExperimentID != "pe-1" {
		t.Errorf("group by = %q, platform experiment = %q, want agent, pe-1", spy.gotGroupBy, spy.gotStats.PlatformExperimentID)
	}
	if code, body := get(t, r, "/experiments/stats?group_by=status"); code != 400 {
		t.Errorf("unknown group_by: status = %d (%s), want 400", code, body)
	}
}

// A coordinator read a baseline job's hypothesis_id, the platform experiment's id, and asked
// for that hypothesis: 404. The list says such a job is on the objective and names no hypothesis.
func TestListExperimentsAnswersABaselineJobAsOnTheObjective(t *testing.T) {
	spy := &filterSpyStore{total: 2, items: []*domain.Experiment{
		{ID: "job-base", PlatformExperimentID: "pe-1", HypothesisID: "pe-1"},
		{ID: "job-own", PlatformExperimentID: "pe-1", HypothesisID: "hyp-1"},
	}}
	code, body := get(t, schedulerRouter(t, spy), "/experiments?platform_experiment_id=pe-1")
	var jobs []map[string]any
	if err := json.Unmarshal([]byte(body), &jobs); code != 200 || err != nil || len(jobs) != 2 {
		t.Fatalf("list: got = %d %s (%v), want 200 with two jobs", code, body, err)
	}
	if _, stated := jobs[0]["hypothesis_id"]; stated || jobs[0]["objective"] != true {
		t.Errorf("the baseline job: got = %v, want objective true and no hypothesis_id", jobs[0])
	}
	if _, stated := jobs[1]["objective"]; stated || jobs[1]["hypothesis_id"] != "hyp-1" {
		t.Errorf("the researcher's job: got = %v, want hypothesis_id hyp-1 and no objective", jobs[1])
	}
}

// One job is answered as an object, which gets a $schema field the list's items do not.
func TestGetExperimentAnswersABaselineJobAsOnTheObjective(t *testing.T) {
	spy := &filterSpyStore{items: []*domain.Experiment{{ID: "job-base", PlatformExperimentID: "pe-1", HypothesisID: "pe-1"}}}
	code, body := get(t, schedulerRouter(t, spy), "/experiments/job-base")
	var job map[string]any
	if err := json.Unmarshal([]byte(body), &job); code != 200 || err != nil {
		t.Fatalf("get: got = %d %s (%v), want 200", code, body, err)
	}
	if _, stated := job["hypothesis_id"]; stated || job["objective"] != true || job["$schema"] == nil {
		t.Errorf("the baseline job: got = %v, want $schema, objective true and no hypothesis_id", job)
	}
	if spy.items[0].HypothesisID != "pe-1" {
		t.Errorf("answering changed the stored job: hypothesis_id = %q, want pe-1", spy.items[0].HypothesisID)
	}
}
