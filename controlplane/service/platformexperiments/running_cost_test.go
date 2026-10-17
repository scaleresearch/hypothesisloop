package platformexperiments

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/metricsdb"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/objectstore"
)

// aliveServer answers ObserveSpan with a job observed continuously from `ago` ago until now —
// enough to give a job a definite, nonzero observed-elapsed time. Other metrics queries (the
// settled-usage lookup) come back empty.
func aliveServer(t *testing.T, now time.Time, step, ago time.Duration) *httptest.Server {
	t.Helper()
	body := fmt.Sprintf(`{"output":[{"records":{"rows":[[%d,%d,%d,%d]]}}]}`,
		now.Add(-ago).UnixMilli(), now.UnixMilli(), ago.Milliseconds(), ago.Milliseconds())
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/v1/sql") {
			_, _ = w.Write([]byte(body))
			return
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
}

// runningExpStore fakes only the one PlatformExperimentsStore method running_cost.go calls;
// every other method panics if ever reached, so a test that exercises an unexpected call fails
// loudly instead of silently returning a zero value.
type runningExpStore struct {
	fakeStore
	running []*domain.Experiment
}

func (f *runningExpStore) GetAgentRunningExperiments(ctx context.Context, agentID, platformExpID string) ([]*domain.Experiment, error) {
	return f.running, nil
}

func newTestService(t *testing.T, metricsURL string, running []*domain.Experiment) *PlatformExperimentsService {
	t.Helper()
	return NewPlatformExperimentsService(
		&runningExpStore{running: running},
		domain.QuotaConfig{},
		zap.NewNop(),
		metricsdb.New(metricsURL),
		3*time.Minute,
	)
}

// newObservedTestService is newTestService but with a logger a test can inspect, for assertions
// that a warning did or did not fire — zap.NewNop() discards everything, which makes "no warning
// fired" untestable.
func newObservedTestService(t *testing.T, metricsURL string, running []*domain.Experiment) (*PlatformExperimentsService, *observer.ObservedLogs) {
	t.Helper()
	core, logs := observer.New(zapcore.WarnLevel)
	svc := NewPlatformExperimentsService(
		&runningExpStore{running: running},
		domain.QuotaConfig{},
		zap.New(core),
		metricsdb.New(metricsURL),
		3*time.Minute,
	)
	return svc, logs
}

// A retired accelerator type (no entry in the live rate catalog — see
// domain.AcceleratorType.LookupCost) must not affect billing at all any more: accH is now derived
// from the job's own admission-time estimate (domain.Experiment.RatedCost), which needs no catalog
// lookup, so it prices identically whether or not the flavor is still registered.
func TestAddRunningActualCostsBillsNormallyWhenAcceleratorRateIsRetired(t *testing.T) {
	domain.SetAcceleratorRates(map[string]float64{"h100": 1.0})
	t.Cleanup(func() { domain.SetAcceleratorRates(map[string]float64{"h100": 1.0}) })

	now := time.Now().UTC()
	server := aliveServer(t, now, time.Minute, 10*time.Minute)
	defer server.Close()

	exp := &domain.Experiment{
		ID:                     "exp-retired",
		CreatedAt:              now.Add(-30 * time.Minute),
		AcceleratorType:        "retired-flavor",
		AcceleratorCount:       2,
		EstimatedDurationHours: 1,
		EstimatedCostAccH:      8,
		CapacityTier:           domain.CapacityGuaranteed,
	}
	svc, logs := newObservedTestService(t, server.URL, []*domain.Experiment{exp})

	q := &domain.AgentQuota{AgentID: "agent-1", PlatformExperimentID: "pe-1"}
	if err := svc.addRunningActualCosts(context.Background(), testPE, q); err != nil {
		t.Fatalf("addRunningActualCosts: %v", err)
	}
	if n := logs.Len(); n != 0 {
		t.Fatalf("got %d warning(s), want 0 — accH no longer depends on the live rate catalog: %v", n, logs.All())
	}

	if q.UsedGuaranteedAccH <= 0 {
		t.Fatalf("used_guaranteed_acch = %v, want > 0 — billed from the admission-time estimate, which needs no catalog rate",
			q.UsedGuaranteedAccH)
	}
}

// correctRunningCosts's delta swap works the same whether or not the flavor is still registered
// in the live rate catalog, since accH no longer depends on it.
func TestCorrectRunningCostsBillsNormallyOnRetiredRate(t *testing.T) {
	domain.SetAcceleratorRates(map[string]float64{"h100": 1.0})
	t.Cleanup(func() { domain.SetAcceleratorRates(map[string]float64{"h100": 1.0}) })

	now := time.Now().UTC()
	server := aliveServer(t, now, time.Minute, 10*time.Minute)
	defer server.Close()

	exp := &domain.Experiment{
		ID:                     "exp-retired",
		CreatedAt:              now.Add(-30 * time.Minute),
		AcceleratorType:        "retired-flavor",
		AcceleratorCount:       2,
		EstimatedDurationHours: 1,
		EstimatedCostAccH:      5,
		CapacityTier:           domain.CapacityGuaranteed,
	}
	svc, logs := newObservedTestService(t, server.URL, []*domain.Experiment{exp})

	q := &domain.AgentQuota{AgentID: "agent-1", PlatformExperimentID: "pe-1", UsedGuaranteedAccH: 5}
	if err := svc.correctRunningCosts(context.Background(), testPE, []*domain.AgentQuota{q}); err != nil {
		t.Fatalf("correctRunningCosts: %v", err)
	}
	if n := logs.Len(); n != 0 {
		t.Fatalf("got %d warning(s), want 0 — accH no longer depends on the live rate catalog: %v", n, logs.All())
	}
	// exp is alive ~30min out of a 1h estimated duration, so RatedCost(5, ~0.5h) ~= 2.5,
	// applied as a delta on top of the pre-seeded 5.
	if q.UsedGuaranteedAccH >= 5 {
		t.Fatalf("used_guaranteed_acch = %v, want < 5 — the observed-so-far cost is below the full estimate",
			q.UsedGuaranteedAccH)
	}
}

// A running CPU job was skipped by the correction and kept its whole estimate as used, though
// settlement bills it by the same rate as any job.
func TestCorrectRunningCostsCountsARunningCPUJobAtItsCostSoFar(t *testing.T) {
	now := time.Now().UTC()
	server := aliveServer(t, now, time.Minute, 10*time.Minute)
	defer server.Close()
	exp := &domain.Experiment{
		ID: "exp-cpu", CreatedAt: now.Add(-30 * time.Minute),
		EstimatedDurationHours: 1, EstimatedCostAccH: 5, CapacityTier: domain.CapacityGuaranteed,
	}
	svc, _ := newObservedTestService(t, server.URL, []*domain.Experiment{exp})
	q := &domain.AgentQuota{AgentID: "agent-1", PlatformExperimentID: "pe-1", UsedGuaranteedAccH: 5}
	if err := svc.correctRunningCosts(context.Background(), testPE, []*domain.AgentQuota{q}); err != nil {
		t.Fatalf("correctRunningCosts: %v", err)
	}
	if q.UsedGuaranteedAccH <= 0 || q.UsedGuaranteedAccH >= 5 {
		t.Fatalf("used_guaranteed_acch = %v, want the half hour it ran, below its estimate of 5", q.UsedGuaranteedAccH)
	}
}

// A job that has never posted an observation must not warn or otherwise short-circuit ahead of
// the !found check.
func TestCorrectRunningCostsNeverObservedIsNotTreatedAsRetiredRate(t *testing.T) {
	domain.SetAcceleratorRates(map[string]float64{"h100": 1.0})
	t.Cleanup(func() { domain.SetAcceleratorRates(map[string]float64{"h100": 1.0}) })

	now := time.Now().UTC()
	// No samples at all: the job has never reported.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/v1/sql") {
			_, _ = w.Write([]byte(`{"output":[{"records":{"rows":[[null,null,null,null]]}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer server.Close()

	exp := &domain.Experiment{
		ID:                     "exp-fresh",
		CreatedAt:              now.Add(-time.Minute),
		AcceleratorType:        "h100", // a real, registered rate
		AcceleratorCount:       1,
		EstimatedDurationHours: 1,
		EstimatedCostAccH:      1,
		CapacityTier:           domain.CapacityGuaranteed,
	}
	svc, logs := newObservedTestService(t, server.URL, []*domain.Experiment{exp})

	q := &domain.AgentQuota{AgentID: "agent-1", PlatformExperimentID: "pe-1", UsedGuaranteedAccH: 1}
	if err := svc.correctRunningCosts(context.Background(), testPE, []*domain.AgentQuota{q}); err != nil {
		t.Fatalf("correctRunningCosts: %v", err)
	}
	if n := logs.Len(); n != 0 {
		t.Fatalf("got %d warning(s) for a job that has simply never reported yet, want 0: %v", n, logs.All())
	}
	if q.UsedGuaranteedAccH != 1 {
		t.Fatalf("used_guaranteed_acch = %v, want unchanged at 1 — an unobserved job must keep its estimate, not be treated as a rate failure",
			q.UsedGuaranteedAccH)
	}
}

var testPE = &domain.PlatformExperiment{ID: "pe-1", CreatedAt: time.Now().UTC().Add(-time.Hour)}

// A requeue settles the interrupted stint into the job's own series while the job lives on, and
// observed elapsed is cumulative over the job's whole life. Once it runs again the first stint is
// in both figures, and must be charged once.
func TestARequeuedJobRunningAgainIsChargedOnceForItsSettledStint(t *testing.T) {
	now := time.Now().UTC()
	const observed = 30 * time.Minute
	sqlBody := fmt.Sprintf(`{"output":[{"records":{"rows":[[%d,%d,%d,%d]]}}]}`,
		now.Add(-observed).UnixMilli(), now.UnixMilli(), observed.Milliseconds(), (10 * time.Minute).Milliseconds())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/v1/sql") {
			_, _ = w.Write([]byte(sqlBody))
			return
		}
		// The first stint, 20 minutes at 8 AccH/h, settled when the job was requeued.
		_, _ = fmt.Fprintf(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"experiment_id":"exp-requeued"},"value":[%d,"2.6666666667"]}]}}`, now.Unix())
	}))
	defer server.Close()

	exp := &domain.Experiment{
		ID:                     "exp-requeued",
		CreatedAt:              now.Add(-time.Hour),
		AcceleratorType:        "h100",
		AcceleratorCount:       2,
		EstimatedDurationHours: 1,
		EstimatedCostAccH:      8,
		CapacityTier:           domain.CapacityGuaranteed,
	}
	svc := newTestService(t, server.URL, []*domain.Experiment{exp})
	const settledStint, lifetime = 8.0 / 3, 4.0 // 30 observed minutes at 8 AccH/h

	observedOnly := &domain.AgentQuota{AgentID: "agent-1", PlatformExperimentID: "pe-1", UsedGuaranteedAccH: settledStint}
	if err := svc.addRunningActualCosts(context.Background(), testPE, observedOnly); err != nil {
		t.Fatalf("addRunningActualCosts: %v", err)
	}
	if got := observedOnly.UsedGuaranteedAccH; got < lifetime-0.01 || got > lifetime+0.01 {
		t.Errorf("observed usage = %v, want %v: the settled stint is counted twice", got, lifetime)
	}

	withReservation := &domain.AgentQuota{AgentID: "agent-1", PlatformExperimentID: "pe-1", UsedGuaranteedAccH: settledStint + exp.EstimatedCostAccH}
	if err := svc.correctRunningCosts(context.Background(), testPE, []*domain.AgentQuota{withReservation}); err != nil {
		t.Fatalf("correctRunningCosts: %v", err)
	}
	if got := withReservation.UsedGuaranteedAccH; got < lifetime-0.01 || got > lifetime+0.01 {
		t.Errorf("usage = %v, want %v: the settled stint is counted twice", got, lifetime)
	}
}

type reservingStore struct {
	runningExpStore
}

func (f *reservingStore) AddDesiredQuotaUsage(_ context.Context, _ string, quotas []*domain.AgentQuota) error {
	for _, q := range quotas {
		for _, job := range f.running {
			q.UsedGuaranteedAccH += job.EstimatedCostAccH
		}
	}
	return nil
}

// A researcher read 0.016 used of 0.029 while two of its jobs ran, beside 0.057 burst hours, and
// was refused a job of 0.0075 at both priorities: a submit holds a running job's whole estimate,
// and burst is spent from the share. The view answers what a submit would.
func TestTheQuotaViewLeavesWhatASubmitWouldAndShowsNoBurstHoursBeyondTheShare(t *testing.T) {
	now := time.Now().UTC()
	srv := aliveServer(t, now, 30*time.Second, 2*time.Minute)
	defer srv.Close()
	running := []*domain.Experiment{{
		ID: "job-1", AgentID: "a", PlatformExperimentID: "pe-1", Status: domain.StatusRunning, CapacityTier: domain.CapacityGuaranteed,
		AcceleratorType: "h100", AcceleratorCount: 1, EstimatedDurationHours: 0.1, EstimatedCostAccH: 0.015, CreatedAt: now.Add(-3 * time.Minute),
	}}
	svc := NewPlatformExperimentsService(&reservingStore{runningExpStore{running: running}}, domain.QuotaConfig{}, zap.NewNop(), metricsdb.New(srv.URL), 3*time.Minute)
	quota := &domain.AgentQuota{AgentID: "a", PlatformExperimentID: "pe-1", GuaranteedAcceleratorHours: 0.028608, BurstAcceleratorHours: 0.057216}
	views, err := svc.quotaViews(context.Background(), &domain.PlatformExperiment{ID: "pe-1", CreatedAt: now.Add(-time.Hour)}, []*domain.AgentQuota{quota})
	if err != nil {
		t.Fatal(err)
	}
	view := views[0]
	if view.UsedGuaranteedAccH >= 0.015 || view.UsedGuaranteedAccH <= 0 {
		t.Errorf("used = %g, want what the running job has spent so far, under its estimate of 0.015", view.UsedGuaranteedAccH)
	}
	held := *quota
	held.UsedGuaranteedAccH = 0.015
	for tier, got := range map[domain.CapacityTier]float64{domain.CapacityGuaranteed: view.RemainingGuaranteed, domain.CapacityBurst: view.RemainingBurst} {
		if want := held.Remaining(tier); got != want || got > 0.0137 {
			t.Errorf("remaining %s = %g, want %g: the share less the whole estimate, as admission counts it", tier, got, want)
		}
	}
	if view.ShareAcceleratorHours != 0.028608 || view.BurstAcceleratorHours != 0.028608 {
		t.Errorf("share %g, burst %g, want both 0.028608: burst hours are part of the share", view.ShareAcceleratorHours, view.BurstAcceleratorHours)
	}
	// A baseline read 0.00016 remaining beside a small used figure: what a running job holds
	// beyond its spend is said, so used, held and remaining add up to the share.
	if want := domain.RoundHours(0.015 - view.UsedGuaranteedAccH); view.HeldAccH != want {
		t.Errorf("held = %g, want %g: the running job's estimate less what it has spent", view.HeldAccH, want)
	}
}

type grantedStore struct {
	reservingStore
}

func (*grantedStore) GetAgentQuota(_ context.Context, agentID, platformExpID string) (*domain.AgentQuota, error) {
	return &domain.AgentQuota{AgentID: agentID, PlatformExperimentID: platformExpID, GuaranteedAcceleratorHours: 1}, nil
}

func (*grantedStore) GetPlatformExperiment(_ context.Context, id string) (*domain.PlatformExperiment, error) {
	return &domain.PlatformExperiment{ID: id, CreatedAt: time.Now().Add(-time.Hour)}, nil
}

// A submit refused with data_quota_exceeded was the only place an agent learned what it holds.
func TestAnAgentsQuotaSaysWhatDataItHoldsAgainstTheCeiling(t *testing.T) {
	metrics := aliveServer(t, time.Now().UTC(), 30*time.Second, 2*time.Minute)
	defer metrics.Close()
	objects := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if prefix := r.URL.Query().Get("prefix"); prefix != objectstore.AgentPrefix("pe-1", "a") {
			t.Errorf("listed %q, want the agent's own prefix", prefix)
		}
		_, _ = w.Write([]byte(`<ListBucketResult><Contents><Key>k1</Key><Size>700</Size></Contents><Contents><Key>k2</Key><Size>300</Size></Contents></ListBucketResult>`))
	}))
	defer objects.Close()
	client, err := objectstore.New("minio", objects.URL, objects.URL, "us-east-1", "hl-data", "root", "root-secret")
	if err != nil {
		t.Fatal(err)
	}
	svc := NewPlatformExperimentsService(&grantedStore{}, domain.QuotaConfig{}, zap.NewNop(), metricsdb.New(metrics.URL), 3*time.Minute)

	if view, err := svc.QuotaView(context.Background(), "a", "pe-1"); err != nil || view.DataBytes != nil || view.MaxDataBytes != 0 {
		t.Fatalf("with no ceiling: got %+v, %v, want no data figures and no read of the store", view, err)
	}
	view, err := svc.WithDataStore(client, 4096).QuotaView(context.Background(), "a", "pe-1")
	if err != nil {
		t.Fatal(err)
	}
	if view.DataBytes == nil || *view.DataBytes != 1000 || view.MaxDataBytes != 4096 {
		t.Errorf("got data_bytes %v of %d, want 1000 of 4096", view.DataBytes, view.MaxDataBytes)
	}
}
