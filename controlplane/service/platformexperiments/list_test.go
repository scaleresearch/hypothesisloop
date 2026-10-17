package platformexperiments

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/platformexperiments/platformexperimentstest"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/metricsdb"
)

// pageStore answers one page of platform experiments and records which ids OnCoresOnly is asked
// about and how often.
type pageStore struct {
	fakeStore
	page    []*domain.PlatformExperiment
	onCores map[string]bool
	asked   [][]string
}

func (s *pageStore) ListPlatformExperiments(context.Context, db.PlatformExperimentsFilter) ([]*domain.PlatformExperiment, error) {
	return s.page, nil
}

func (s *pageStore) CountPlatformExperiments(context.Context, db.PlatformExperimentsFilter) (int, error) {
	return len(s.page), nil
}

func (s *pageStore) OnCoresOnly(_ context.Context, ids []string) (map[string]bool, error) {
	s.asked = append(s.asked, slices.Clone(ids))
	out := map[string]bool{}
	for _, id := range ids {
		if s.onCores[id] {
			out[id] = true
		}
	}
	return out, nil
}

// spendServer is a metrics store that answers every query with spent, by platform experiment,
// and counts the queries.
func spendServer(t *testing.T, spent map[string]float64) (string, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var queries []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		queries = append(queries, r.URL.Query().Get("query"))
		mu.Unlock()
		type sample struct {
			Metric map[string]string `json:"metric"`
			Value  [2]any            `json:"value"`
		}
		result := []sample{}
		for id, hours := range spent {
			result = append(result, sample{Metric: map[string]string{"platform_experiment_id": id}, Value: [2]any{float64(time.Now().Unix()), strconv.FormatFloat(hours, 'f', -1, 64)}})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": result}})
	}))
	t.Cleanup(server.Close)
	return server.URL, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(queries)
	}
}

// A page of platform experiments is answered with each one's spend from one read of the metrics
// store, however many it holds, and none for a draft, which runs no job. Each says what its budget
// counts: the kind of the type its proposal was sized for, and without a proposal cpu only once it
// has jobs and runs every one on cores, which the store is asked once for the whole page.
func TestAPageIsAnsweredWithItsSpendFromOneMetricsReadAndEachBudgetKind(t *testing.T) {
	created := time.Now().UTC().Add(-48 * time.Hour)
	proposal := func(acceleratorType domain.AcceleratorType) json.RawMessage {
		raw, err := json.Marshal(map[string]any{"accelerator_type": acceleratorType})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	store := &pageStore{
		page: []*domain.PlatformExperiment{
			{ID: "pe-gpu-proposal", Status: domain.PlatformExpRunning, CreatedAt: created, Proposal: proposal("nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3")},
			{ID: "pe-cpu-proposal", Status: domain.PlatformExpRunning, CreatedAt: created, Proposal: proposal(domain.CPUType)},
			{ID: "pe-no-jobs", Status: domain.PlatformExpOpen, CreatedAt: created},
			{ID: "pe-on-cores", Status: domain.PlatformExpRunning, CreatedAt: created},
			{ID: "pe-mixed", Status: domain.PlatformExpClosed, CreatedAt: created},
			{ID: "pe-draft", Status: domain.PlatformExpDraft, CreatedAt: created},
		},
		onCores: map[string]bool{"pe-on-cores": true},
	}
	url, queries := spendServer(t, map[string]float64{"pe-gpu-proposal": 2.5, "pe-on-cores": 4})
	svc := NewService(store, platformexperimentstest.Settings, zap.NewNop(), metricsdb.New(url), 3*time.Minute)

	listed, total, err := svc.List(context.Background(), ListQuery{Limit: 50})
	if err != nil || total != len(store.page) || len(listed) != len(store.page) {
		t.Fatalf("List = %d of %d, %v", len(listed), total, err)
	}
	if got := queries(); len(got) != 1 || !strings.Contains(got[0], "sum by (platform_experiment_id)") {
		t.Fatalf("metrics queries = %q, want one for the whole page", got)
	}
	if got := store.asked; len(got) != 1 || !slices.Equal(got[0], []string{"pe-no-jobs", "pe-on-cores", "pe-mixed", "pe-draft"}) {
		t.Fatalf("OnCoresOnly asked %v, want once, for the platform experiments with no proposal", got)
	}
	want := map[string]struct {
		kind     domain.TypeKind
		consumed *float64
	}{
		"pe-gpu-proposal": {domain.TypeKindAccelerator, ptr(2.5)},
		"pe-cpu-proposal": {domain.TypeKindCPU, ptr(0)},
		"pe-no-jobs":      {domain.TypeKindAccelerator, ptr(0)},
		"pe-on-cores":     {domain.TypeKindCPU, ptr(4)},
		"pe-mixed":        {domain.TypeKindAccelerator, ptr(0)},
		"pe-draft":        {domain.TypeKindAccelerator, nil},
	}
	for _, pe := range listed {
		w := want[pe.ID]
		if pe.BudgetKind != w.kind {
			t.Errorf("%s: budget_kind %q, want %q", pe.ID, pe.BudgetKind, w.kind)
		}
		switch {
		case w.consumed == nil && pe.ConsumedAcceleratorHours != nil:
			t.Errorf("%s: consumed %v, want none for a draft", pe.ID, *pe.ConsumedAcceleratorHours)
		case w.consumed != nil && (pe.ConsumedAcceleratorHours == nil || *pe.ConsumedAcceleratorHours != *w.consumed):
			t.Errorf("%s: consumed %v, want %v", pe.ID, pe.ConsumedAcceleratorHours, *w.consumed)
		}
	}
}

// A page of drafts alone asks the metrics store nothing.
func TestAPageOfDraftsReadsNoSpend(t *testing.T) {
	store := &pageStore{page: []*domain.PlatformExperiment{{ID: "pe-draft", Status: domain.PlatformExpDraft}}}
	url, queries := spendServer(t, nil)
	svc := NewService(store, platformexperimentstest.Settings, zap.NewNop(), metricsdb.New(url), 3*time.Minute)
	if _, _, err := svc.List(context.Background(), ListQuery{Limit: 50}); err != nil {
		t.Fatal(err)
	}
	if got := queries(); len(got) != 0 {
		t.Fatalf("metrics queries = %q, want none", got)
	}
}

func ptr(v float64) *float64 { return &v }
