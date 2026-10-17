package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/platformexperiments"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/platformexperiments/platformexperimentstest"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
	"go.uber.org/zap"
)

type noExperimentStore struct{ fakeStore }

func (noExperimentStore) GetPlatformExperiment(context.Context, string) (*domain.PlatformExperiment, error) {
	return nil, nil
}

func (noExperimentStore) ListAgentQuotas(context.Context, string) ([]*domain.AgentQuota, error) {
	return nil, nil
}

func TestQuotasOfAnUnknownPlatformExperimentAreNotFound(t *testing.T) {
	r := authtest.Router(authtest.Staff)
	doc := apidocs.New(r, "test", "1.0.0", "", auth.NewAccess(authtest.Placement{}), zap.NewNop())
	svc := newSignupTestService(t, noExperimentStore{})
	registerPlatformExperiments(doc, svc.WithRepositories(&fakeRepositories{}).WithPlanning(testPlanning))

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, authtest.Authorize(httptest.NewRequest(http.MethodGet, "/platform-experiments/unknown/quotas", nil)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body)
	}
}

// pagedExperiments records the limit each list was read with.
type pagedExperiments struct {
	fakeStore
	limits *[]int
}

func (s pagedExperiments) ListPlatformExperiments(_ context.Context, filter db.PlatformExperimentsFilter) ([]*domain.PlatformExperiment, error) {
	*s.limits = append(*s.limits, filter.Limit)
	return nil, nil
}

func (pagedExperiments) CountPlatformExperiments(context.Context, db.PlatformExperimentsFilter) (int, error) {
	return 0, nil
}

// The list shortened a limit over the maximum to it, so a short page read as the whole set.
func TestThePlatformExperimentListRefusesAPageOverTheMaximum(t *testing.T) {
	var read []int
	r := authtest.Router(authtest.Staff)
	doc := apidocs.New(r, "test", "1.0.0", "", auth.NewAccess(authtest.Placement{}), zap.NewNop())
	registerPlatformExperiments(doc, newSignupTestService(t, pagedExperiments{limits: &read}).WithRepositories(&fakeRepositories{}).WithPlanning(testPlanning))

	for _, list := range []string{"/platform-experiments"} {
		for _, query := range []string{"limit=201", "limit=100000", "limit=-1", "offset=-1"} {
			if code, out := call(t, r, "GET", list+"?"+query, ""); code != http.StatusUnprocessableEntity || out["reason"] != "invalid_page" {
				t.Errorf("GET %s?%s: got = %d %v, want 422 invalid_page", list, query, code, out)
			}
		}
		if len(read) != 0 {
			t.Fatalf("GET %s: a refused page was read from the store with limits %v", list, read)
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, authtest.Authorize(httptest.NewRequest(http.MethodGet, list+"?limit=200", nil)))
		if rec.Code != http.StatusOK || len(read) != 1 || read[0] != 200 {
			t.Errorf("GET %s?limit=200: got = %d, read with %v, want 200 and the store asked for 200: %s", list, rec.Code, read, rec.Body)
		}
		read = nil
	}
}

// summarizedStore is statusStore with the write of a summary, which fails while broken is set.
type summarizedStore struct {
	statusStore
	broken error
}

func (s *summarizedStore) SetPlatformExperimentSummary(_ context.Context, id, summary string) error {
	if s.broken != nil {
		return s.broken
	}
	for _, pe := range s.created {
		if pe.ID == id {
			if record := s.records[id]; record != "" {
				summary = strings.TrimLeft(summary+"\n\n"+record, "\n")
			}
			pe.Summary = summary
			return nil
		}
	}
	return db.ErrPlatformExperimentNotFound
}

func (s *summarizedStore) NameActors(context.Context, []*domain.Actor) error { return nil }

// Closing and the summary answered every failure 400 with the error's own text, a database
// error among them, and a second close read as bad input. Each answers the platform experiment
// it changed, a refusal by its status and reason, and a failure of the platform says nothing.
func TestCloseAndSummaryAnswerThePlatformExperimentAndRefuseByReason(t *testing.T) {
	store := &summarizedStore{statusStore: statusStore{createdStore: createdStore{created: []*domain.PlatformExperiment{
		{ID: "pe-1", ProjectID: "prj-a", Status: domain.PlatformExpDraft, Goal: "g", Stages: []domain.Stage{{LengthPct: 100}}},
	}}}}
	svc := newSignupTestService(t, store)
	placement := authtest.Placement{PlatformExperiments: map[string]domain.Tenancy{"pe-1": {PlatformExperimentID: "pe-1", ProjectID: "prj-a", OrgID: "org-a"}}}
	r := authtest.Router(authtest.As(waitingPerson))
	doc := apidocs.New(r, "test", "1.0.0", "", auth.NewAccess(placement), zap.NewNop())
	registerPlatformExperiments(doc, svc.WithRepositories(&fakeRepositories{}).WithPlanning(testPlanning))

	if code, out := call(t, r, "PUT", "/platform-experiments/pe-1/summary", `{"summary":"what was found"}`); code != http.StatusOK || out["id"] != "pe-1" || out["summary"] != "what was found" {
		t.Errorf("a summary: got = %d %v, want 200 and the platform experiment with it", code, out)
	}
	if code, _ := call(t, r, "POST", "/platform-experiments/pe-1/summary", `{"summary":"again"}`); code != http.StatusMethodNotAllowed {
		t.Errorf("a summary sent with POST: got = %d, want 405", code)
	}
	store.broken = errors.New("pq: connection refused at 10.0.0.7")
	if code, out := call(t, r, "PUT", "/platform-experiments/pe-1/summary", `{"summary":"lost"}`); code != http.StatusInternalServerError || strings.Contains(fmt.Sprint(out), "10.0.0.7") {
		t.Errorf("a summary the store fails: got = %d %v, want 500 with no internal text", code, out)
	}
	store.broken = nil

	if code, out := call(t, r, "POST", "/platform-experiments/pe-1/close", ""); code != http.StatusOK || out["id"] != "pe-1" || out["status"] != "closed" {
		t.Errorf("a close: got = %d %v, want 200 and the platform experiment as closed", code, out)
	}
	if code, out := call(t, r, "POST", "/platform-experiments/pe-1/close", ""); code != http.StatusConflict || out["reason"] != "invalid_transition" {
		t.Errorf("a second close: got = %d %v, want 409 invalid_transition", code, out)
	}
	for name, err := range map[string]error{
		"closing one that does not exist":      func() error { _, err := svc.Close(context.Background(), "pe-none", nil, "", nil); return err }(),
		"a summary of one that does not exist": func() error { _, err := svc.SetSummary(context.Background(), "pe-none", "s"); return err }(),
	} {
		var refused *refusal.Refusal
		if !errors.As(err, &refused) || refused.GetStatus() != http.StatusNotFound || refused.Reason != "not_found" {
			t.Errorf("%s: got = %v, want 404 not_found", name, err)
		}
	}
	_, err := svc.SetSummary(context.Background(), "pe-none", strings.Repeat("x", domain.MaxProseBytes+1))
	var refused *refusal.Refusal
	if !errors.As(err, &refused) || refused.GetStatus() != http.StatusUnprocessableEntity || refused.Reason != "summary_too_long" {
		t.Errorf("a summary too long: got = %v, want 422 summary_too_long", err)
	}
}

type constraintResultsStore struct{ fakeStore }

func (constraintResultsStore) GetPlatformExperiment(context.Context, string) (*domain.PlatformExperiment, error) {
	bound := 0.0
	return &domain.PlatformExperiment{ID: "pe-1", Status: domain.PlatformExpRunning, Metrics: []domain.MetricDefinition{
		{Key: "digest_mismatches", Direction: "minimize", Role: domain.MetricRoleConstraint, Bound: &bound},
	}}, nil
}

func (constraintResultsStore) CompletedJobIDs(context.Context, string) (map[string]bool, error) {
	return map[string]bool{}, nil
}

// The metrics store refusing a query is the platform's failure. Answered as 404, it told agents
// the platform experiment had no results and sent its internal error text along.
func TestResultsTheMetricsStoreRefusesAreAnInternalErrorAndNotNotFound(t *testing.T) {
	metricsDB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"status":"error","error":"invalid promql query","errorType":"InvalidArguments"}`))
	}))
	defer metricsDB.Close()
	r := authtest.Router(authtest.Staff)
	doc := apidocs.New(r, "test", "1.0.0", "", auth.NewAccess(authtest.Placement{}), zap.NewNop())
	svc := reaching(platformexperiments.NewPlatformExperimentsService(constraintResultsStore{}, platformexperimentstest.Settings, zap.NewNop(), metricsDB.URL, 3*time.Minute), domain.CPUType)
	registerPlatformExperiments(doc, svc.WithRepositories(&fakeRepositories{}).WithPlanning(testPlanning))

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, authtest.Authorize(httptest.NewRequest(http.MethodGet, "/platform-experiments/pe-1/results", nil)))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "promql") || strings.Contains(rec.Body.String(), "metricsdb") {
		t.Fatalf("the answer carries internal text: %s", rec.Body)
	}
}
