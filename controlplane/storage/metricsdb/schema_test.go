package metricsdb

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// emptyStore is the test GreptimeDB with every table dropped. It is a throwaway instance of its
// own: make test starts one, never the stack's.
func emptyStore(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("HYPOTHESISLOOP_TEST_METRICS_DB_URL")
	if url == "" {
		url = "http://localhost:4011"
	}
	if resp, err := http.Get(url + "/health"); err != nil {
		t.Skipf("no GreptimeDB at %s: %v", url, err)
	} else {
		resp.Body.Close()
	}
	s := New(url)
	ctx := context.Background()
	rows, err := s.querySQLRows(ctx, `SELECT table_name FROM information_schema.tables WHERE table_schema = 'public' AND table_name <> '`+physicalTable+`'`)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range append(rows, []any{physicalTable}) {
		if err := s.execSQL(ctx, `DROP TABLE IF EXISTS "`+r[0].(string)+`"`); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func TestEnsureSchemaCreatesEveryDeclaredTableAndNothingElse(t *testing.T) {
	s := emptyStore(t)
	ctx := context.Background()
	if err := s.EnsureSchema(ctx); err != nil {
		t.Fatalf("first start: got = %v, want nil", err)
	}
	if err := s.EnsureSchema(ctx); err != nil {
		t.Fatalf("next start: got = %v, want nil", err)
	}
	rows, err := s.querySQLRows(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public'`)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := rows[0][0], float64(len(schema())); got != want {
		t.Fatalf("tables: got = %v, want %v", got, want)
	}
}

// Every writer, with every label it may write, writes only what the registry declares: the
// store refuses anything else, and the schema is still exactly the registry's after.
func TestEveryWriterWritesOnlyDeclaredTablesAndLabels(t *testing.T) {
	s := emptyStore(t)
	ctx := context.Background()
	if err := s.EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Truncate(time.Millisecond)
	one := 1.0
	writes := map[string]error{
		"observations": s.RecordObservations(ctx, []Observation{
			{ExperimentID: "job", At: at, ExtraLabels: map[string]string{"pod_uid": "pod", "node": "n"}},
			{ExperimentID: "job", At: at, ExtraLabels: map[string]string{"cluster_name": "c", "node": "n"}},
		}),
		"admission": s.RecordAdmission(ctx, "job", at),
		"job metric": s.RecordJobMetric(ctx, JobMetric{JobID: "job", PlatformExperimentID: "pe", AgentID: "a", Name: "loss", Basis: "raw",
			Value: 1, FractionComplete: 0.5, At: at}),
		"job statuses": s.RecordJobStatuses(ctx, "c", at, []JobStatusSample{{ExperimentID: "job", ClusterName: "c", Phase: domain.JobPhaseRunning.String(),
			AdmittedAcceleratorType: "h100", AdmittedNode: "n", At: at,
			AdmittedMachine: &domain.JobMachine{InstanceID: "i", HostID: "h", AcceleratorModel: "m", CPUModel: "cpu", PricePerHourUSD: 2}}}),
		"cluster capacity": s.RecordClusterCapacitySnapshot(ctx, ClusterCapacitySnapshot{ClusterName: "c", At: at, CPUAvailable: 1, CPUTotal: 2,
			AcceleratorAvailable: map[string]int64{"h100": 1}, AcceleratorTotal: map[string]int64{"h100": 2},
			AcceleratorAvailableByNode: map[string]map[string]int64{"n": {"h100": 1}},
			NodeResourcesByNode:        map[string]map[string]int64{"n": {domain.NodeResourceCPUMillicores: 1}},
			NodeResourcesTotalByNode:   map[string]map[string]int64{"n": {domain.NodeResourceCPUMillicores: 2}},
			NodeLabelsByNode:           map[string]map[string]string{"n": {"zone": "a"}},
			ClusterID:                  "id", AgentVersion: "v", MultiNodeCapable: true, AutoscalerEnabled: true, RAMAvailable: 1, RAMTotal: 2, StorageAvailable: 1, StorageTotal: 2}),
		"run health": s.RecordRunHealth(ctx, at, []domain.AgentHealth{
			{PlatformExperimentID: "pe", AgentID: "a", Role: "executor", FirstJobSeconds: &one, FirstCompletedSeconds: &one, JobsInFlight: 1},
			{PlatformExperimentID: "pe", Role: "coordinator"},
		}),
		"harness state": s.RecordHarnessState(ctx, at, []domain.HarnessState{{PlatformExperimentID: "pe", AgentID: "a", Role: "executor",
			Subagents: 1, TurnSince: at, InstructionsDigest: "abcdef", AgentType: "researcher", AgentTypeVersion: 2,
			Model: domain.RateLimit{Rejected: true, Utilization: &one, ResetsAt: &at}}}),
		"observed cost": s.SetObservedBatch(ctx, "a", "pe", "job", domain.CapacityGuaranteed, map[domain.ResourceType]float64{domain.ResourceAcceleratorHours: 1}),
		"log tail":      s.RecordLogTail(ctx, "job", "c", []string{"line"}, at),
		"phase detail":  s.RecordPhaseDetail(ctx, "job", "c", "reason", "message", 1, 1, 1, at),
		"model call": s.RecordModelCall(ctx, ModelCall{OrgID: "o", PlatformExperimentID: "pe", AgentID: "a", Provider: "anthropic", Model: "m",
			InputTokens: 1, At: at}),
	}
	for name, err := range writes {
		if err != nil {
			t.Errorf("%s: got = %v, want nil", name, err)
		}
	}
	if err := s.EnsureSchema(ctx); err != nil {
		t.Fatalf("after every writer: got = %v, want the registry's schema unchanged", err)
	}
}

func TestEnsureSchemaRefusesAnUndeclaredColumnOrTable(t *testing.T) {
	ctx := context.Background()
	for name, stray := range map[string]struct{ ddl, named string }{
		"a column": {`ALTER TABLE experiment_admitted ADD COLUMN "stray" STRING PRIMARY KEY`, "unexpected experiment_admitted.stray string TAG"},
		"a table":  {`CREATE TABLE stray (ts TIMESTAMP(3) TIME INDEX)`, "unexpected stray.ts timestamp(3) TIMESTAMP"},
	} {
		s := emptyStore(t)
		if err := s.EnsureSchema(ctx); err != nil {
			t.Fatal(err)
		}
		if err := s.execSQL(ctx, stray.ddl); err != nil {
			t.Fatal(err)
		}
		if err := s.EnsureSchema(ctx); err == nil || !strings.Contains(err.Error(), stray.named) {
			t.Errorf("%s: got = %v, want a refusal naming %q", name, err, stray.named)
		}
	}
}

func TestEnsureSchemaRefusesAMissingColumn(t *testing.T) {
	s := emptyStore(t)
	ctx := context.Background()
	if err := s.execSQL(ctx, physical().create); err != nil {
		t.Fatal(err)
	}
	if err := s.execSQL(ctx, metricTable(aliveHeartbeatMetric, "experiment_id").create); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureSchema(ctx); err == nil || !strings.Contains(err.Error(), "missing "+aliveHeartbeatMetric+".pod_uid string TAG") {
		t.Fatalf("got = %v, want a refusal naming the missing column", err)
	}
}

// GreptimeDB creates a table, or a column, for whatever remote write names, unless the write
// says not to: the store refuses both, so nothing appears that EnsureSchema did not create.
func TestTheStoreRefusesARemoteWriteOfAnUndeclaredTableOrColumn(t *testing.T) {
	s := emptyStore(t)
	ctx := context.Background()
	if err := s.EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}
	for name, g := range map[string]gauge{
		"a table":  {table: metricTable("undeclared_metric", "experiment_id"), labels: map[string]string{"experiment_id": "job"}},
		"a column": {table: metricTable(admissionMetric, "experiment_id", "stray"), labels: map[string]string{"experiment_id": "job", "stray": "x"}},
	} {
		g.value, g.at = 1, time.Now()
		if err := s.writeGauges(ctx, []gauge{g}); err == nil {
			t.Errorf("%s: got = nil, want the store to refuse it", name)
		}
	}
	if err := s.EnsureSchema(ctx); err != nil {
		t.Fatalf("after the refused writes: got = %v, want the registry's schema unchanged", err)
	}
}

func TestWriteGaugesRefusesALabelItsTableDoesNotDeclare(t *testing.T) {
	err := New("http://unused.invalid").writeGauges(context.Background(), []gauge{{table: admission, labels: map[string]string{"stray": "x"}, value: 1, at: time.Now()}})
	if err == nil || !strings.Contains(err.Error(), "declares no label stray") {
		t.Fatalf("got = %v, want a refusal naming the label", err)
	}
}
