package controller

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// fakeMetricsStore answers the handful of GreptimeDB queries checkSilence makes, routed by what
// each one is asking rather than by call order — so a change in the order of checks does not
// silently turn these tests into something else.
//
// windowSamples is how many samples the declared metric posted inside the silence window;
// lifetimeSamples how many it has posted over the job's whole life. windowSamples=0 with
// lifetimeSamples>0 is "reported earlier, quiet now" — the case the never-reported verdict must
// NOT claim.
type fakeMetricsStore struct {
	aliveSince      time.Time
	phase           int // 2 = running, per metricsdb.LatestJobPhase
	windowSamples   float64
	lifetimeSamples float64
	spread          float64
}

func (f *fakeMetricsStore) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/v1/sql"):
			sql := r.URL.Query().Get("sql")
			switch {
			case strings.Contains(sql, "LAG(ts)"):
				// ObserveSpan: {first, last, total ms, stint ms}. A job observed across the
				// whole silence window, which is what a live job looks like.
				observedFor := time.Since(f.aliveSince)
				fmt.Fprintf(w, `{"output":[{"records":{"rows":[[%d,%d,%d,%d]]}}]}`,
					f.aliveSince.UnixMilli(), time.Now().UTC().UnixMilli(),
					observedFor.Milliseconds(), observedFor.Milliseconds())
			case strings.Contains(sql, "CROSS JOIN present"):
				// ClusterSnapshotPresence: {newest snapshot, last present, absent count}.
				now := float64(time.Now().UnixMilli()) / 1000
				fmt.Fprintf(w, `{"output":[{"records":{"rows":[[%v,%v,0]]}}]}`, now, now)
			default:
				// LatestJobPhase: one row of {snapshot, phase, observed_at}. Phase and observed_at
				// must agree with the snapshot timestamp or the reader rejects it.
				fmt.Fprintf(w, `{"output":[{"records":{"rows":[[1000,%d,1000]]}}]}`, f.phase)
			}
			return
		case strings.Contains(r.URL.Query().Get("query"), "count_over_time"):
			// declaredMetricSpread's first query. The lifetime lookback is much longer than the
			// silence window, which is how the two calls are told apart.
			count := f.windowSamples
			if isLifetimeWindow(r.URL.Query().Get("query")) {
				count = f.lifetimeSamples
			}
			fmt.Fprintf(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"%v"]}]}}`, count)
			return
		case strings.Contains(r.URL.Query().Get("query"), "max_over_time"):
			fmt.Fprintf(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"%v"]}]}}`, f.spread)
			return
		}
		// Everything else is a liveness / first-observation read. The two use different endpoints
		// and demand different result types: IsAlive runs an instant query (vector), FirstObserved
		// a range query (matrix), and each rejects the other's shape.
		if strings.Contains(r.URL.Path, "query_range") {
			writeAliveMatrix(w, f.aliveSince)
			return
		}
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"1"]}]}}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// isLifetimeWindow distinguishes the "has it ever reported" query from the in-window one by its
// range duration: checkSilence asks the second over the job's whole life, which is far longer.
func isLifetimeWindow(query string) bool {
	open := strings.LastIndex(query, "[")
	close := strings.LastIndex(query, "]")
	if open < 0 || close < open {
		return false
	}
	var seconds int
	fmt.Sscanf(query[open+1:close], "%ds", &seconds)
	return seconds > 600
}

func writeAliveMatrix(w http.ResponseWriter, since time.Time) {
	var values []string
	for ts := since.Unix(); ts <= time.Now().UTC().Unix(); ts += 60 {
		values = append(values, fmt.Sprintf(`[%d,"1"]`, ts))
	}
	if len(values) == 0 {
		values = append(values, fmt.Sprintf(`[%d,"1"]`, since.Unix()))
	}
	fmt.Fprintf(w, `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[%s]}]}}`,
		strings.Join(values, ","))
}

func silenceController(url string) *Controller {
	c := &Controller{
		metricsDBURL:          url,
		logger:                zap.NewNop(),
		silenceMultiplier:     3,
		defaultReportInterval: 10 * time.Second,
		minSilenceWindow:      10 * time.Second,
	}
	return c
}

func runningExperiment() *domain.Experiment {
	return &domain.Experiment{
		ID:                   "exp-mute",
		ClusterName:          "cluster-a",
		PlatformExperimentID: "pe-1",
		Status:               domain.StatusRunning,
	}
}

// A live job that has never emitted a metric its platform experiment declared cannot be ranked,
// cut or compared — there is nothing to judge it by — while it holds an accelerator and bills for
// it. It is evicted, and named for the actual fault: its reporting path, not a hung trainer.
func TestCheckSilenceEvictsLiveJobThatNeverReportedADeclaredMetric(t *testing.T) {
	store := &fakeMetricsStore{
		aliveSince:      time.Now().UTC().Add(-30 * time.Minute),
		phase:           2,
		windowSamples:   0,
		lifetimeSamples: 0,
	}
	c := silenceController(store.start(t).URL)

	evict, reason, err := c.checkSilence(context.Background(), runningExperiment(), time.Now().UTC(), nil, []string{"val_accuracy"})
	if err != nil {
		t.Fatalf("checkSilence: %v", err)
	}
	if !evict {
		t.Fatal("a live job that never reported its declared metric was not evicted — nothing can judge it, and it bills for an accelerator meanwhile")
	}
	if reason != domain.EvictionNeverReportedMetrics {
		t.Fatalf("eviction reason = %q, want %q: the fault is the reporting path, not a hung trainer",
			reason, domain.EvictionNeverReportedMetrics)
	}
}

// The trap this verdict must not fall into: "no samples in the silence window" and "never
// reported at all" are different jobs. One reported an hour ago and went quiet — a hung trainer,
// judged elsewhere — and calling that "never reported" would both evict on the wrong evidence and
// tell the agent to go fix a reporting path that works.
func TestCheckSilenceDoesNotCallAQuietJobNeverReported(t *testing.T) {
	store := &fakeMetricsStore{
		aliveSince:      time.Now().UTC().Add(-30 * time.Minute),
		phase:           2,
		windowSamples:   0, // nothing lately...
		lifetimeSamples: 5, // ...but it reported earlier in its life
	}
	c := silenceController(store.start(t).URL)

	_, reason, err := c.checkSilence(context.Background(), runningExperiment(), time.Now().UTC(), nil, []string{"val_accuracy"})
	if err != nil {
		t.Fatalf("checkSilence: %v", err)
	}
	if reason == domain.EvictionNeverReportedMetrics {
		t.Fatal("a job that reported earlier and went quiet was condemned as never having reported")
	}
}

// The single-sample case, which the progress reader deliberately folds into "not reported": one
// point cannot prove movement. It IS a complete answer to "did this job ever report", though, so a
// job that emitted exactly one metric must not be condemned as never having reported.
func TestCheckSilenceDoesNotCallAJobWithOneSampleNeverReported(t *testing.T) {
	store := &fakeMetricsStore{
		aliveSince:      time.Now().UTC().Add(-30 * time.Minute),
		phase:           2,
		windowSamples:   0,
		lifetimeSamples: 1, // exactly one, ever
	}
	c := silenceController(store.start(t).URL)

	_, reason, err := c.checkSilence(context.Background(), runningExperiment(), time.Now().UTC(), nil, []string{"val_accuracy"})
	if err != nil {
		t.Fatalf("checkSilence: %v", err)
	}
	if reason == domain.EvictionNeverReportedMetrics {
		t.Fatal("a job that emitted exactly one declared metric was condemned as never having reported")
	}
}

// The pre-existing verdict this rewrite must not have dropped: a job whose declared metric keeps
// arriving but never moves is a hung training loop re-emitting a cached value. It looks perfectly
// alive to any presence-only check, which is exactly why the metric contract exists.
func TestCheckSilenceEvictsAJobWhoseDeclaredMetricStoppedMoving(t *testing.T) {
	store := &fakeMetricsStore{
		aliveSince:      time.Now().UTC().Add(-30 * time.Minute),
		phase:           2,
		windowSamples:   5, // reporting steadily...
		lifetimeSamples: 5,
		spread:          0.0, // ...at one constant value
	}
	c := silenceController(store.start(t).URL)

	evict, reason, err := c.checkSilence(context.Background(), runningExperiment(), time.Now().UTC(), nil, []string{"val_accuracy"})
	if err != nil {
		t.Fatalf("checkSilence: %v", err)
	}
	if !evict {
		t.Fatal("a job re-emitting one constant value was not evicted — a hung trainer looks alive forever")
	}
	if reason != domain.EvictionSilent {
		t.Fatalf("eviction reason = %q, want %q: it reported, so the fault is the trainer, not the reporting path",
			reason, domain.EvictionSilent)
	}
}

// A job whose declared metric is arriving and moving is working, however little it has produced.
func TestCheckSilenceSparesAJobWhoseDeclaredMetricIsMoving(t *testing.T) {
	store := &fakeMetricsStore{
		aliveSince:      time.Now().UTC().Add(-30 * time.Minute),
		phase:           2,
		windowSamples:   5,
		lifetimeSamples: 5,
		spread:          0.25,
	}
	c := silenceController(store.start(t).URL)

	evict, _, err := c.checkSilence(context.Background(), runningExperiment(), time.Now().UTC(), nil, []string{"val_accuracy"})
	if err != nil {
		t.Fatalf("checkSilence: %v", err)
	}
	if evict {
		t.Fatal("a job reporting a moving declared metric was evicted")
	}
}

// A platform experiment declaring no ranking metric has nothing to hold a job to, so silence
// detection has no contract to enforce and must not invent one.
func TestCheckSilenceSparesJobWhenNoMetricWasDeclared(t *testing.T) {
	store := &fakeMetricsStore{aliveSince: time.Now().UTC().Add(-30 * time.Minute), phase: 2}
	c := silenceController(store.start(t).URL)

	evict, _, err := c.checkSilence(context.Background(), runningExperiment(), time.Now().UTC(), nil, nil)
	if err != nil {
		t.Fatalf("checkSilence: %v", err)
	}
	if evict {
		t.Fatal("a job was evicted for not reporting a metric its platform experiment never declared")
	}
}
