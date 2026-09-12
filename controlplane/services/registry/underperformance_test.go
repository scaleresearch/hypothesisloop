package registry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// underperformStore answers GetPlatformExperimentMetrics with one fixed ranking metric and
// GetExperiment with the given experiment. Everything else panics if called — this check touches
// nothing more of Store.
type underperformStore struct {
	Store
	exp     *domain.Experiment
	metrics []domain.MetricDefinition
}

func (s underperformStore) GetExperiment(_ context.Context, _ string) (*domain.Experiment, error) {
	return s.exp, nil
}

func (s underperformStore) GetPlatformExperimentMetrics(_ context.Context, _ string) ([]domain.MetricDefinition, bool, error) {
	return s.metrics, true, nil
}

// capturingNotifier records every event handed to it, standing in for the Postgres NOTIFY path
// this check ultimately drives in production.
type capturingNotifier struct {
	events []db.Event
}

func (n *capturingNotifier) NotifyEvent(_ context.Context, e db.Event) error {
	n.events = append(n.events, e)
	return nil
}

// cohortSeriesServer answers a PromQL range query with one series for "exp-1" (the job under
// test, one point older than the 2s "just written" cutoff) and three sibling series, regardless
// of the actual query string — the same shortcut recordedSamplesServer uses in metrics_test.go.
func cohortSeriesServer(selfPrevValue float64, siblingValues []float64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		selfTS := time.Now().UTC().Add(-time.Hour).Unix()  // old: must predate the 2s "just written" cutoff
		sibTS := time.Now().UTC().Add(-time.Minute).Unix() // recent: must survive the sibling staleness bound
		type series struct {
			Metric map[string]string `json:"metric"`
			Values [][2]interface{}  `json:"values"`
		}
		result := []series{
			{Metric: map[string]string{"job_id": "exp-1", "agent_id": "agent-1", "metric_name": "acc"},
				Values: [][2]interface{}{{selfTS, strconv.FormatFloat(selfPrevValue, 'f', -1, 64)}}},
		}
		for i, v := range siblingValues {
			result = append(result, series{
				Metric: map[string]string{"job_id": "sib-" + strconv.Itoa(i), "agent_id": "agent-" + strconv.Itoa(i), "metric_name": "acc"},
				Values: [][2]interface{}{{sibTS, strconv.FormatFloat(v, 'f', -1, 64)}},
			})
		}
		body := map[string]any{"status": "success", "data": map[string]any{"resultType": "matrix", "result": result}}
		enc, _ := json.Marshal(body)
		_, _ = w.Write(enc)
	}))
}

// staleCohortServer is like cohortSeriesServer but every sibling's last point is well past
// underperformSiblingStaleness — a cohort that stopped reporting a while ago, not a live one.
func staleCohortServer(selfPrevValue float64, siblingValues []float64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		selfTS := time.Now().UTC().Add(-time.Hour).Unix()
		staleTS := time.Now().UTC().Add(-2 * time.Hour).Unix()
		type series struct {
			Metric map[string]string `json:"metric"`
			Values [][2]interface{}  `json:"values"`
		}
		result := []series{
			{Metric: map[string]string{"job_id": "exp-1", "agent_id": "agent-1", "metric_name": "acc"},
				Values: [][2]interface{}{{selfTS, strconv.FormatFloat(selfPrevValue, 'f', -1, 64)}}},
		}
		for i, v := range siblingValues {
			result = append(result, series{
				Metric: map[string]string{"job_id": "sib-" + strconv.Itoa(i), "agent_id": "agent-" + strconv.Itoa(i), "metric_name": "acc"},
				Values: [][2]interface{}{{staleTS, strconv.FormatFloat(v, 'f', -1, 64)}},
			})
		}
		body := map[string]any{"status": "success", "data": map[string]any{"resultType": "matrix", "result": result}}
		enc, _ := json.Marshal(body)
		_, _ = w.Write(enc)
	}))
}

func rankingMetric(key, direction string) []domain.MetricDefinition {
	return []domain.MetricDefinition{{Key: key, Direction: direction}}
}

// A job whose previous checkpoint was ahead of the pack and whose current one is clearly cut off
// from the worst of the rest crosses into the advisory: the agent hears about it once, this call.
func TestUnderperformanceFiresOnceOnTheTransitionIntoTrailing(t *testing.T) {
	server := cohortSeriesServer(0.87, []float64{0.9, 0.85, 0.88})
	defer server.Close()

	notifier := &capturingNotifier{}
	svc := &Service{
		store:        underperformStore{exp: &domain.Experiment{ID: "exp-1", AgentID: "agent-1", PlatformExperimentID: "pe-1"}, metrics: rankingMetric("acc", "maximize")},
		metricsDBURL: server.URL,
		logger:       zap.NewNop(),
		events:       notifier,
	}

	svc.checkUnderperformance(context.Background(), &domain.Experiment{ID: "exp-1", AgentID: "agent-1", PlatformExperimentID: "pe-1"}, "acc", 0.5, 0.5, time.Now().UTC())

	if len(notifier.events) != 1 {
		t.Fatalf("events = %d, want 1", len(notifier.events))
	}
	got := notifier.events[0]
	if got.Kind != db.EventJobUnderperforming || got.Subject != "exp-1" || got.Value != "acc" {
		t.Fatalf("event = %+v, want kind=%s subject=exp-1 value=acc", got, db.EventJobUnderperforming)
	}
}

// A job that was already trailing as of its own previous checkpoint is in the same stretch, not
// a new one — no second advisory for it.
func TestUnderperformanceDoesNotRepeatWithinTheSameStretch(t *testing.T) {
	server := cohortSeriesServer(0.60, []float64{0.9, 0.85, 0.88}) // already trailing last checkpoint too
	defer server.Close()

	notifier := &capturingNotifier{}
	svc := &Service{
		store:        underperformStore{exp: &domain.Experiment{ID: "exp-1", AgentID: "agent-1", PlatformExperimentID: "pe-1"}, metrics: rankingMetric("acc", "maximize")},
		metricsDBURL: server.URL,
		logger:       zap.NewNop(),
		events:       notifier,
	}

	svc.checkUnderperformance(context.Background(), &domain.Experiment{ID: "exp-1", AgentID: "agent-1", PlatformExperimentID: "pe-1"}, "acc", 0.5, 0.5, time.Now().UTC())

	if len(notifier.events) != 0 {
		t.Fatalf("events = %d, want 0 (already trailing last checkpoint)", len(notifier.events))
	}
}

// A cohort smaller than underperformCohortMinSize gives no signal to compare against, so nothing
// fires no matter how bad the value looks.
func TestUnderperformanceNeedsAMinimumCohort(t *testing.T) {
	server := cohortSeriesServer(0.87, []float64{0.9}) // only one sibling
	defer server.Close()

	notifier := &capturingNotifier{}
	svc := &Service{
		store:        underperformStore{exp: &domain.Experiment{ID: "exp-1", AgentID: "agent-1", PlatformExperimentID: "pe-1"}, metrics: rankingMetric("acc", "maximize")},
		metricsDBURL: server.URL,
		logger:       zap.NewNop(),
		events:       notifier,
	}

	svc.checkUnderperformance(context.Background(), &domain.Experiment{ID: "exp-1", AgentID: "agent-1", PlatformExperimentID: "pe-1"}, "acc", 0.5, 0.5, time.Now().UTC())

	if len(notifier.events) != 0 {
		t.Fatalf("events = %d, want 0 (cohort too small)", len(notifier.events))
	}
}

// A sibling that stopped reporting a while ago is a different phase of the run, not a fair
// live comparison — it must not count toward the cohort, even if its last value looked great.
func TestUnderperformanceIgnoresStaleSiblings(t *testing.T) {
	server := staleCohortServer(0.87, []float64{0.9, 0.85, 0.88})
	defer server.Close()

	notifier := &capturingNotifier{}
	svc := &Service{
		store:        underperformStore{exp: &domain.Experiment{ID: "exp-1", AgentID: "agent-1", PlatformExperimentID: "pe-1"}, metrics: rankingMetric("acc", "maximize")},
		metricsDBURL: server.URL,
		logger:       zap.NewNop(),
		events:       notifier,
	}

	svc.checkUnderperformance(context.Background(), &domain.Experiment{ID: "exp-1", AgentID: "agent-1", PlatformExperimentID: "pe-1"}, "acc", 0.5, 0.5, time.Now().UTC())

	if len(notifier.events) != 0 {
		t.Fatalf("events = %d, want 0 (siblings all stale, cohort too small)", len(notifier.events))
	}
}

// Below underperformMinFraction, one or two early samples can rank last by chance alone, so the
// check does not even query the cohort.
func TestUnderperformanceSkipsEarlyProgress(t *testing.T) {
	server := cohortSeriesServer(0.87, []float64{0.9, 0.85, 0.88})
	defer server.Close()

	notifier := &capturingNotifier{}
	svc := &Service{
		store:        underperformStore{exp: &domain.Experiment{ID: "exp-1", AgentID: "agent-1", PlatformExperimentID: "pe-1"}, metrics: rankingMetric("acc", "maximize")},
		metricsDBURL: server.URL,
		logger:       zap.NewNop(),
		events:       notifier,
	}

	svc.checkUnderperformance(context.Background(), &domain.Experiment{ID: "exp-1", AgentID: "agent-1", PlatformExperimentID: "pe-1"}, "acc", 0.05, 0.01, time.Now().UTC())

	if len(notifier.events) != 0 {
		t.Fatalf("events = %d, want 0 (below min fraction)", len(notifier.events))
	}
}

// A cohort that happens to be reporting the exact same value has no spread to take a fraction
// of; a naive margin of underperformSeparation*(best-worst) is then 0, which would call out any
// nonzero gap at all, however negligible. This was caught live: three siblings all reporting 0.9
// flagged a self value of 0.88 as "trailing" even though it was 2% off a tied field.
func TestUnderperformanceDoesNotFlagATinyGapAgainstATiedCohort(t *testing.T) {
	server := cohortSeriesServer(0.95, []float64{0.9, 0.9, 0.9})
	defer server.Close()

	notifier := &capturingNotifier{}
	svc := &Service{
		store:        underperformStore{exp: &domain.Experiment{ID: "exp-1", AgentID: "agent-1", PlatformExperimentID: "pe-1"}, metrics: rankingMetric("acc", "maximize")},
		metricsDBURL: server.URL,
		logger:       zap.NewNop(),
		events:       notifier,
	}

	svc.checkUnderperformance(context.Background(), &domain.Experiment{ID: "exp-1", AgentID: "agent-1", PlatformExperimentID: "pe-1"}, "acc", 0.5, 0.88, time.Now().UTC())

	if len(notifier.events) != 0 {
		t.Fatalf("events = %d, want 0 (2%% gap against a tied cohort is not a real straggler)", len(notifier.events))
	}
}

// Two checkpoints written a second apart are still two distinct checkpoints. This was caught
// live: a coarse "ignore anything from the last 2 seconds" cutoff discarded the previous, genuine
// checkpoint as if it were the sample just written, so the edge-check had nothing to compare
// against and fired on every sample of a stretch instead of only its first one.
func TestUnderperformanceEdgeCheckIsNotFooledByCloselyTimedCheckpoints(t *testing.T) {
	// Both siblings and self report at nearly the same instant; self's previous checkpoint
	// already trailed by exactly the margin of its current one, so the edge should not fire
	// again here even though both samples land inside what used to be the 2s cutoff window.
	server := cohortSeriesServer(0.3, []float64{0.9, 0.85, 0.88})
	defer server.Close()

	notifier := &capturingNotifier{}
	svc := &Service{
		store:        underperformStore{exp: &domain.Experiment{ID: "exp-1", AgentID: "agent-1", PlatformExperimentID: "pe-1"}, metrics: rankingMetric("acc", "maximize")},
		metricsDBURL: server.URL,
		logger:       zap.NewNop(),
		events:       notifier,
	}

	// The self series' one stored point (0.3, timestamped an hour ago by cohortSeriesServer) is
	// older than `at`, so it is read back as the previous checkpoint regardless of how close
	// together the two RecordMetric calls actually were in wall-clock time.
	svc.checkUnderperformance(context.Background(), &domain.Experiment{ID: "exp-1", AgentID: "agent-1", PlatformExperimentID: "pe-1"}, "acc", 0.6, 0.3, time.Now().UTC())

	if len(notifier.events) != 0 {
		t.Fatalf("events = %d, want 0 (same stretch as the previous checkpoint, however close in time)", len(notifier.events))
	}
}
