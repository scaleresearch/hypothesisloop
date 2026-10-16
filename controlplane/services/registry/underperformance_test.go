package registry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
)

// underperformStore answers GetPlatformExperimentMetrics with one fixed ranking metric and
// GetExperiment with the given experiment, and records marks the way the experiments row does:
// the first mark of a job is the job.underperforming event, a later one writes nothing.
// Everything else panics if called.
type underperformStore struct {
	Store
	exp     *domain.Experiment
	metrics []domain.MetricDefinition
	marks   *marks
	// completed is the siblings that ran to a successful end.
	completed map[string]bool
}

func (s underperformStore) CompletedJobIDs(context.Context, string) (map[string]bool, error) {
	return s.completed, nil
}

type marks struct{ events []domain.Event }

func (s underperformStore) MarkUnderperforming(_ context.Context, id, metricName string) error {
	for _, e := range s.marks.events {
		if e.Subject == id {
			return nil
		}
	}
	s.marks.events = append(s.marks.events, domain.Event{Kind: domain.EventJobUnderperforming, Subject: id, Value: metricName})
	return nil
}

func (s underperformStore) GetExperiment(_ context.Context, _ string) (*domain.Experiment, error) {
	return s.exp, nil
}

func (s underperformStore) GetPlatformExperimentMetrics(_ context.Context, _ string) ([]domain.MetricDefinition, bool, error) {
	return s.metrics, true, nil
}

// cohort is the metrics store the check reads. reporting is how long ago the job under test
// reported its first sample, at fraction 0: the pace its remaining run is told from.
type cohort struct {
	*httptest.Server
	reporting time.Duration
}

type promSeries struct {
	Metric map[string]string `json:"metric"`
	Values [][2]interface{}  `json:"values"`
}

// ownSeries is what the job under test reported before the sample being checked.
func (c *cohort) ownSeries(fractionQuery bool) promSeries {
	v := "0.5"
	if fractionQuery {
		v = "0"
	}
	return promSeries{Metric: map[string]string{"job_id": "exp-1", "agent_id": "agent-1", "metric_name": "acc"},
		Values: [][2]interface{}{{time.Now().UTC().Add(-c.reporting).Unix(), v}}}
}

func (c *cohort) serve(series func(fractionQuery bool) []promSeries) *cohort {
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = r.ParseForm()
		fractionQuery := strings.Contains(r.Form.Get("query"), metricsdb.ExperimentMetricFraction)
		result := append(series(fractionQuery), c.ownSeries(fractionQuery))
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "matrix", "result": result}})
	}))
	return c
}

// cohortServer answers the two range queries of the check, value and fraction_complete, with one
// series per sibling and the job's own. Each sibling reported at every tenth of its run, the last
// sample `age` ago, and valueAt gives its value at a fraction. The job has reported for an hour.
func cohortServer(age time.Duration, siblings int, valueAt func(sibling int, fraction float64) float64) *cohort {
	end := time.Now().UTC().Add(-age).Unix()
	return (&cohort{reporting: time.Hour}).serve(func(fractionQuery bool) []promSeries {
		var result []promSeries
		for i := 0; i < siblings; i++ {
			s := promSeries{Metric: map[string]string{"job_id": "sib-" + strconv.Itoa(i), "agent_id": "agent-" + strconv.Itoa(i), "metric_name": "acc"}}
			for tenth := 1; tenth <= 10; tenth++ {
				fraction := float64(tenth) / 10
				v := fraction
				if !fractionQuery {
					v = valueAt(i, fraction)
				}
				s.Values = append(s.Values, [2]interface{}{end - int64(10-tenth), strconv.FormatFloat(v, 'f', -1, 64)})
			}
			result = append(result, s)
		}
		return result
	})
}

// cohortSeriesServer is a live cohort whose siblings each held one value all run long.
func cohortSeriesServer(siblingValues []float64) *cohort {
	return cohortServer(time.Minute, len(siblingValues), func(i int, _ float64) float64 { return siblingValues[i] })
}

// staleCohortServer is the same cohort with every sibling's last sample well past
// underperformSiblingStaleness.
func staleCohortServer(siblingValues []float64) *cohort {
	return cohortServer(2*time.Hour, len(siblingValues), func(i int, _ float64) float64 { return siblingValues[i] })
}

// theJob is the job under test, admitted a day ago: when it was admitted is no part of its pace.
func theJob() *domain.Experiment {
	admitted := time.Now().UTC().Add(-24 * time.Hour)
	return &domain.Experiment{ID: "exp-1", AgentID: "agent-1", PlatformExperimentID: "pe-1", SubmittedAt: &admitted}
}

func rankingMetric(key, direction string) []domain.MetricDefinition {
	return []domain.MetricDefinition{{Key: key, Direction: direction}}
}

// A job launched after its siblings finished trails them from its first sample: its previous
// sample trailed as well. It is told at its first checkpoint, and once, however many follow.
func TestUnderperformanceFiresOnceOnAJobThatTrailsFromItsFirstSample(t *testing.T) {
	server := cohortSeriesServer([]float64{0.9, 0.9, 0.9125, 0.9375, 0.94375, 0.875})
	defer server.Close()

	notifier := &marks{}
	svc := &Service{
		store:        underperformStore{exp: theJob(), metrics: rankingMetric("acc", "maximize"), marks: notifier},
		metricsDBURL: server.URL,
		logger:       zap.NewNop(),
	}

	// A run of 3 minutes: 36 s in at 20%, 54 s at 30%, 72 s at 40%, where under 2 minutes are left.
	for fraction, elapsed := range map[float64]time.Duration{0.2: 36 * time.Second, 0.3: 54 * time.Second, 0.4: 72 * time.Second} {
		server.reporting = elapsed
		svc.checkUnderperformance(context.Background(), theJob(), "acc", fraction, 0.84375, time.Now().UTC())
	}

	if len(notifier.events) != 1 {
		t.Fatalf("events = %d, want 1", len(notifier.events))
	}
	got := notifier.events[0]
	if got.Kind != domain.EventJobUnderperforming || got.Subject != "exp-1" || got.Value != "acc" {
		t.Fatalf("event = %+v, want kind=%s subject=exp-1 value=acc", got, domain.EventJobUnderperforming)
	}
}

// A job at 0.5 beside four siblings near 0.9 trails them, unless those numbers rank nowhere: a
// sibling that completed and broke the gate is no cohort, and one still running is not judged.
func TestUnderperformanceLeavesOutCompletedSiblingsThatBrokeAConstraint(t *testing.T) {
	siblings := cohortSeriesServer([]float64{0.9, 0.9, 0.9125, 0.9375})
	defer siblings.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if !strings.Contains(r.Form.Get("query"), `metric_name="exact_match"`) {
			siblings.Config.Handler.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[` +
			`{"metric":{"job_id":"sib-0","metric_basis":"raw"},"value":[1,"0.97"]},` +
			`{"metric":{"job_id":"sib-1","metric_basis":"raw"},"value":[1,"0.97"]},` +
			`{"metric":{"job_id":"sib-2","metric_basis":"raw"},"value":[1,"0.97"]},` +
			`{"metric":{"job_id":"sib-3","metric_basis":"raw"},"value":[1,"1"]}]}}`))
	}))
	defer server.Close()

	bound := 1.0
	gated := append(rankingMetric("acc", "maximize"), domain.MetricDefinition{Key: "exact_match", Direction: "maximize", Role: domain.MetricRoleConstraint, Bound: &bound})
	for name, c := range map[string]struct {
		completed map[string]bool
		events    int
	}{
		"three of four completed and broke the gate": {map[string]bool{"sib-0": true, "sib-1": true, "sib-2": true, "sib-3": true}, 0},
		"the same three still run":                   {map[string]bool{"sib-3": true}, 1},
	} {
		notifier := &marks{}
		svc := &Service{
			store:        underperformStore{exp: theJob(), metrics: gated, marks: notifier, completed: c.completed},
			metricsDBURL: server.URL,
			logger:       zap.NewNop(),
		}
		svc.checkUnderperformance(context.Background(), theJob(), "acc", 0.5, 0.5, time.Now().UTC())
		if len(notifier.events) != c.events {
			t.Errorf("%s: events = %d, want %d", name, len(notifier.events), c.events)
		}
	}
}

// A cohort smaller than underperformCohortMinSize gives no signal to compare against, so nothing
// fires no matter how bad the value looks.
func TestUnderperformanceNeedsAMinimumCohort(t *testing.T) {
	server := cohortSeriesServer([]float64{0.9}) // only one sibling
	defer server.Close()

	notifier := &marks{}
	svc := &Service{
		store:        underperformStore{exp: theJob(), metrics: rankingMetric("acc", "maximize"), marks: notifier},
		metricsDBURL: server.URL,
		logger:       zap.NewNop(),
	}

	svc.checkUnderperformance(context.Background(), theJob(), "acc", 0.5, 0.5, time.Now().UTC())

	if len(notifier.events) != 0 {
		t.Fatalf("events = %d, want 0 (cohort too small)", len(notifier.events))
	}
}

// A sibling that stopped reporting a while ago is a different phase of the run, not a fair
// live comparison — it must not count toward the cohort, even if its last value looked great.
func TestUnderperformanceIgnoresStaleSiblings(t *testing.T) {
	server := staleCohortServer([]float64{0.9, 0.85, 0.88})
	defer server.Close()

	notifier := &marks{}
	svc := &Service{
		store:        underperformStore{exp: theJob(), metrics: rankingMetric("acc", "maximize"), marks: notifier},
		metricsDBURL: server.URL,
		logger:       zap.NewNop(),
	}

	svc.checkUnderperformance(context.Background(), theJob(), "acc", 0.5, 0.5, time.Now().UTC())

	if len(notifier.events) != 0 {
		t.Fatalf("events = %d, want 0 (siblings all stale, cohort too small)", len(notifier.events))
	}
}

// Below underperformMinFraction, one or two early samples can rank last by chance alone, so the
// check does not even query the cohort.
func TestUnderperformanceSkipsEarlyProgress(t *testing.T) {
	server := cohortSeriesServer([]float64{0.9, 0.85, 0.88})
	defer server.Close()

	notifier := &marks{}
	svc := &Service{
		store:        underperformStore{exp: theJob(), metrics: rankingMetric("acc", "maximize"), marks: notifier},
		metricsDBURL: server.URL,
		logger:       zap.NewNop(),
	}

	svc.checkUnderperformance(context.Background(), theJob(), "acc", 0.05, 0.01, time.Now().UTC())

	if len(notifier.events) != 0 {
		t.Fatalf("events = %d, want 0 (below min fraction)", len(notifier.events))
	}
}

// A cohort that happens to be reporting the exact same value has no spread to take a fraction
// of; a naive margin of underperformSeparation*(best-worst) is then 0, which would call out any
// nonzero gap at all, however negligible. This was caught live: three siblings all reporting 0.9
// flagged a self value of 0.88 as "trailing" even though it was 2% off a tied field.
func TestUnderperformanceDoesNotFlagATinyGapAgainstATiedCohort(t *testing.T) {
	server := cohortSeriesServer([]float64{0.9, 0.9, 0.9})
	defer server.Close()

	notifier := &marks{}
	svc := &Service{
		store:        underperformStore{exp: theJob(), metrics: rankingMetric("acc", "maximize"), marks: notifier},
		metricsDBURL: server.URL,
		logger:       zap.NewNop(),
	}

	svc.checkUnderperformance(context.Background(), theJob(), "acc", 0.5, 0.88, time.Now().UTC())

	if len(notifier.events) != 0 {
		t.Fatalf("events = %d, want 0 (2%% gap against a tied cohort is not a real straggler)", len(notifier.events))
	}
}

// A job about to end gives its agent nothing to save by cancelling it: half done after 20 seconds
// of reporting leaves 20 seconds, less than the agent needs to read the advisory. The same half
// after an hour leaves an hour, and fires. The pace is the job's own: how long ago it was
// admitted says nothing, since a job on a rented machine waits minutes for it to start.
func TestUnderperformanceStaysSilentWhenTheJobIsAboutToEnd(t *testing.T) {
	server := cohortSeriesServer([]float64{0.9, 0.85, 0.88})
	defer server.Close()

	for _, tc := range []struct {
		reporting time.Duration
		fraction  float64
		want      int
	}{{20 * time.Second, 0.5, 0}, {time.Hour, 0.5, 1}, {10 * time.Minute, 0.9, 0}, {10 * time.Hour, 0.9, 1}} {
		notifier := &marks{}
		job := theJob()
		svc := &Service{
			store:        underperformStore{exp: job, metrics: rankingMetric("acc", "maximize"), marks: notifier},
			metricsDBURL: server.URL,
			logger:       zap.NewNop(),
		}
		server.reporting = tc.reporting
		svc.checkUnderperformance(context.Background(), job, "acc", tc.fraction, 0.5, time.Now().UTC())
		if len(notifier.events) != tc.want {
			t.Fatalf("reporting for %s at fraction %v: events = %d, want %d", tc.reporting, tc.fraction, len(notifier.events), tc.want)
		}
	}
}

// On a metric that climbs all run long a job at 30% is below every finished sibling's final
// value. It is measured against where they stood at 30%, so keeping pace is not trailing and
// falling behind that point is.
func TestUnderperformanceComparesSiblingsAtTheJobsOwnProgress(t *testing.T) {
	climbing := func(i int, fraction float64) float64 { return fraction + 0.01*float64(i) }
	server := cohortServer(time.Minute, 4, climbing)
	defer server.Close()

	for _, tc := range []struct {
		value float64
		want  int
	}{{0.3, 0}, {0.1, 1}} {
		notifier := &marks{}
		svc := &Service{
			store:        underperformStore{exp: theJob(), metrics: rankingMetric("acc", "maximize"), marks: notifier},
			metricsDBURL: server.URL,
			logger:       zap.NewNop(),
		}
		svc.checkUnderperformance(context.Background(), theJob(), "acc", 0.3, tc.value, time.Now().UTC())
		if len(notifier.events) != tc.want {
			t.Fatalf("value %v at 30%%: events = %d, want %d", tc.value, len(notifier.events), tc.want)
		}
	}
}

// A sibling that has not come as far as the job has nothing to be compared at, so a cohort of
// such siblings is no cohort.
func TestUnderperformanceLeavesOutSiblingsBehindTheJob(t *testing.T) {
	server := (&cohort{reporting: time.Hour}).serve(func(fractionQuery bool) []promSeries {
		v := "0.9"
		if fractionQuery {
			v = "0.2"
		}
		ts := time.Now().UTC().Add(-time.Minute).Unix()
		var result []promSeries
		for i := 0; i < 4; i++ {
			result = append(result, promSeries{
				Metric: map[string]string{"job_id": "sib-" + strconv.Itoa(i), "agent_id": "agent-" + strconv.Itoa(i), "metric_name": "acc"},
				Values: [][2]interface{}{{ts, v}},
			})
		}
		return result
	})
	defer server.Close()

	notifier := &marks{}
	svc := &Service{
		store:        underperformStore{exp: theJob(), metrics: rankingMetric("acc", "maximize"), marks: notifier},
		metricsDBURL: server.URL,
		logger:       zap.NewNop(),
	}
	svc.checkUnderperformance(context.Background(), theJob(), "acc", 0.6, 0.1, time.Now().UTC())
	if len(notifier.events) != 0 {
		t.Fatalf("events = %d, want 0 (no sibling has reached 60%%)", len(notifier.events))
	}
}
