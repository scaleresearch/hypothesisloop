package metricsdb

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// physicalTable holds the rows of every metric table, each a logical table on it.
const physicalTable = "greptime_physical_table"

// table is one of the store's tables, as the registry below declares it. Only the registry makes
// one, so a writer cannot name a table EnsureSchema did not create.
type table struct {
	name    string
	create  string
	columns []column
	tags    map[string]bool
}

// column is a column as information_schema.columns describes it.
type column struct {
	name, dataType, semantic string
}

func tag(name string) column             { return column{name, "string", "TAG"} }
func field(name, dataType string) column { return column{name, dataType, "FIELD"} }
func timeIndex(name string) column       { return column{name, "timestamp(3)", "TIMESTAMP"} }

// newTable is the table of these columns, created with options after its column list. Every
// name is quoted: a label may be an SQL keyword (resource).
func newTable(name, options string, columns ...column) *table {
	t := &table{name: name, columns: columns, tags: map[string]bool{}}
	var lines, key []string
	var index string
	for _, c := range columns {
		null := "NULL"
		switch c.semantic {
		case "TAG":
			t.tags[c.name] = true
			key = append(key, `"`+c.name+`"`)
		case "TIMESTAMP":
			index, null = c.name, "NOT NULL"
		}
		lines = append(lines, fmt.Sprintf("\t%q %s %s", c.name, strings.ToUpper(c.dataType), null))
	}
	lines = append(lines, fmt.Sprintf("\tTIME INDEX (%q)", index))
	if len(key) > 0 {
		lines = append(lines, "\tPRIMARY KEY ("+strings.Join(key, ", ")+")")
	}
	t.create = "CREATE TABLE IF NOT EXISTS " + name + " (\n" + strings.Join(lines, ",\n") + "\n) " + options
	return t
}

// metricTable is a metric's logical table, with the labels it is written with as its tags.
func metricTable(name string, tags ...string) *table {
	columns := []column{timeIndex("greptime_timestamp"), field("greptime_value", "double")}
	for _, t := range tags {
		columns = append(columns, tag(t))
	}
	return newTable(name, `ENGINE=metric WITH ("on_physical_table" = "`+physicalTable+`")`, columns...)
}

// The metric tables. Every metric any writer emits is one of these, with every label it may carry.
var (
	aliveHeartbeat     = metricTable(aliveHeartbeatMetric, "experiment_id", "pod_uid", "node", "cluster_name")
	admission          = metricTable(admissionMetric, "experiment_id")
	experimentMetric   = metricTable(ExperimentMetricValue, jobMetricTags...)
	experimentFraction = metricTable(ExperimentMetricFraction, jobMetricTags...)
	experimentNode     = metricTable(experimentNodeMetric, "experiment_id", "node",
		machineInstanceLabel, machineHostLabel, machineAcceleratorLabel, machineCPULabel, machinePriceLabel)
	acceleratorType = metricTable(acceleratorTypeMetric, "experiment_id", "accelerator_type")
	usedHours       = metricTable(usedHoursMetric, "agent_id", "platform_experiment_id", "experiment_id", "resource_type", "tier", "kind")

	clusterJobSnapshot   = metricTable(clusterJobSnapshotMetric, "cluster_name", "sample_kind")
	clusterJobPhase      = metricTable(clusterJobPhaseMetric, "cluster_name", "experiment_id", "sample_kind")
	clusterJobObservedAt = metricTable(clusterJobObservedAtMetric, "cluster_name", "experiment_id", "sample_kind")

	clusterHeartbeat                = metricTable(clusterHeartbeatMetric, "agent_version", "cluster_id", "cluster_name")
	clusterCPUAvailable             = metricTable(clusterCPUAvailableMetric, "cluster_name")
	clusterCPUTotal                 = metricTable(clusterCPUTotalMetric, "cluster_name")
	clusterRAMAvailable             = metricTable(clusterRAMAvailableMetric, "cluster_name")
	clusterRAMTotal                 = metricTable(clusterRAMTotalMetric, "cluster_name")
	clusterStorageAvailable         = metricTable(clusterStorageAvailableMetric, "cluster_name")
	clusterStorageTotal             = metricTable(clusterStorageTotalMetric, "cluster_name")
	clusterMultiNodeCapable         = metricTable(clusterMultiNodeCapableMetric, "cluster_name")
	clusterAutoscalerEnabled        = metricTable(clusterAutoscalerEnabledMetric, "cluster_name")
	clusterAcceleratorAvailable     = metricTable(clusterAcceleratorAvailableMetric, "accelerator_type", "cluster_name")
	clusterAcceleratorTotal         = metricTable(clusterAcceleratorTotalMetric, "accelerator_type", "cluster_name")
	clusterNodeAcceleratorAvailable = metricTable(clusterNodeAcceleratorAvailableMetric, "accelerator_type", "cluster_name", "node")
	clusterNodeResourceAvailable    = metricTable(clusterNodeResourceAvailableMetric, "cluster_name", "node", "resource")
	clusterNodeResourceTotal        = metricTable(clusterNodeResourceTotalMetric, "cluster_name", "node", "resource")
	clusterNodeLabel                = metricTable(clusterNodeLabelMetric, "cluster_name", "label_key", "label_value", "node")

	agentFirstJobSeconds          = metricTable(agentFirstJobSecondsMetric, healthTags...)
	agentFirstCompletedJobSeconds = metricTable(agentFirstCompletedJobSecondsMetric, healthTags...)
	agentJobsInFlight             = metricTable(agentJobsInFlightMetric, healthTags...)
	agentRestarts                 = metricTable(agentRestartsMetric, healthTags...)
	agentCoordinatorMessages      = metricTable(agentCoordinatorMessagesMetric, healthTags...)
	agentStateReportedAt          = metricTable(agentStateReportedAtMetric, healthTags...)
	agentSubagents                = metricTable(agentSubagentsMetric, healthTags...)
	agentTurnWaiting              = metricTable(agentTurnWaitingMetric, healthTags...)
	agentTurnSince                = metricTable(agentTurnSinceMetric, healthTags...)
	agentInstructionsDigest       = metricTable(agentInstructionsDigestMetric, healthTags...)
	agentInstructionsVersion      = metricTable(agentInstructionsVersionMetric, append(healthTags, "agent_type")...)
	agentRateLimitUtilization     = metricTable(agentRateLimitUtilizationMetric, healthTags...)
	agentRateLimitRejected        = metricTable(agentRateLimitRejectedMetric, healthTags...)
	agentRateLimitResetsAt        = metricTable(agentRateLimitResetsAtMetric, healthTags...)
)

var (
	jobMetricTags = []string{"agent_id", "job_id", "metric_basis", "metric_name", "platform_experiment_id"}
	healthTags    = []string{"agent_id", "platform_experiment_id", "role"}
)

var metricTables = []*table{
	aliveHeartbeat, admission, experimentMetric, experimentFraction, experimentNode, acceleratorType, usedHours,
	clusterJobSnapshot, clusterJobPhase, clusterJobObservedAt,
	clusterHeartbeat, clusterCPUAvailable, clusterCPUTotal, clusterRAMAvailable, clusterRAMTotal,
	clusterStorageAvailable, clusterStorageTotal, clusterMultiNodeCapable, clusterAutoscalerEnabled,
	clusterAcceleratorAvailable, clusterAcceleratorTotal, clusterNodeAcceleratorAvailable,
	clusterNodeResourceAvailable, clusterNodeResourceTotal, clusterNodeLabel,
	agentFirstJobSeconds, agentFirstCompletedJobSeconds, agentJobsInFlight, agentRestarts, agentCoordinatorMessages,
	agentStateReportedAt, agentSubagents, agentTurnWaiting, agentTurnSince, agentInstructionsDigest,
	agentInstructionsVersion, agentRateLimitUtilization, agentRateLimitRejected, agentRateLimitResetsAt,
}

// The tables written through SQL, for what is not a number. job_logs and job_phase_detail keep
// one row per (experiment_id, ts), the latest a job's current read. phase_message, not message:
// GreptimeDB's SQL parser reserves message. model_calls holds one row per model call a sandboxed
// agent made through the model proxy, with its cost; append_mode keeps two calls of one agent on
// one model in the same millisecond as two rows, where a primary key and a timestamp would keep
// the last.
var sqlTables = []*table{
	newTable("job_logs", "WITH (ttl = '30d')",
		tag("experiment_id"), field("cluster_name", "string"), field("lines_json", "string"), timeIndex("ts")),
	newTable("job_phase_detail", "WITH (ttl = '30d')",
		tag("experiment_id"), field("cluster_name", "string"), field("reason", "string"), field("phase_message", "string"),
		field("restart_count", "bigint"), field("scheduled_nodes", "bigint"),
		field("attempt", "bigint"), timeIndex("ts")),
	modelCalls,
}

var modelCalls = newTable("model_calls", "WITH (append_mode = 'true')",
	tag("org_id"), tag("platform_experiment_id"), tag("agent_id"), tag("provider"), tag("model"),
	field("input_tokens", "bigint"), field("output_tokens", "bigint"),
	field("cache_creation_input_tokens", "bigint"), field("cache_read_input_tokens", "bigint"), field("cost_usd", "double"), timeIndex("ts"))

// physical is the table every metric table lives on. Its tags are the union of theirs, which
// GreptimeDB adds as it creates them, beside the two it keys rows by.
func physical() *table {
	t := newTable(physicalTable, `ENGINE=metric WITH ("physical_metric_table" = "")`,
		timeIndex("greptime_timestamp"), field("greptime_value", "double"))
	t.columns = append(t.columns, column{"__table_id", "int unsigned", "TAG"}, column{"__tsid", "bigint unsigned", "TAG"})
	tags := map[string]bool{}
	for _, m := range metricTables {
		maps.Copy(tags, m.tags)
	}
	for _, name := range slices.Sorted(maps.Keys(tags)) {
		t.columns = append(t.columns, tag(name))
	}
	return t
}

// schema is every table of the store, the physical table first.
func schema() []*table {
	return slices.Concat([]*table{physical()}, metricTables, sqlTables)
}

// EnsureSchema creates every table the registry declares, moves the model calls of a store from
// before model_calls into it, priced at anthropic where they were recorded with no cost
// (moveModelUsage), then refuses unless the store holds exactly those tables with exactly their
// columns. Run once at startup by control-service alone.
func (s *Store) EnsureSchema(ctx context.Context, anthropic domain.ModelCatalog) error {
	tables := schema()
	want := map[string]bool{}
	for _, t := range tables {
		if err := s.execSQL(ctx, t.create); err != nil {
			return fmt.Errorf("metricsdb.EnsureSchema: create %s: %w", t.name, err)
		}
		for _, c := range t.columns {
			want[t.name+"."+c.name+" "+c.dataType+" "+c.semantic] = true
		}
	}
	if err := s.moveModelUsage(ctx, anthropic); err != nil {
		return fmt.Errorf("metricsdb.EnsureSchema: %w", err)
	}
	rows, err := s.querySQLRows(ctx, `SELECT table_name, column_name, data_type, semantic_type FROM information_schema.columns WHERE table_schema = 'public'`)
	if err != nil {
		return fmt.Errorf("metricsdb.EnsureSchema: read the schema: %w", err)
	}
	have := map[string]bool{}
	for _, r := range rows {
		if len(r) != 4 {
			return fmt.Errorf("metricsdb.EnsureSchema: read the schema: a row of %d columns", len(r))
		}
		have[fmt.Sprintf("%v.%v %v %v", r[0], r[1], r[2], r[3])] = true
	}
	var differences []string
	for _, c := range slices.Sorted(maps.Keys(want)) {
		if !have[c] {
			differences = append(differences, "missing "+c)
		}
	}
	for _, c := range slices.Sorted(maps.Keys(have)) {
		if !want[c] {
			differences = append(differences, "unexpected "+c)
		}
	}
	if len(differences) > 0 {
		return fmt.Errorf("metricsdb.EnsureSchema: the store is not the schema this release declares: %s", strings.Join(differences, "; "))
	}
	return nil
}

// moveModelUsage copies every call of model_usage, the table a store from before model_calls
// holds them in, into model_calls, and drops model_usage. A call recorded with no cost, all of
// them anthropic's, is priced at anthropic, and one of a model anthropic does not price is refused
// before anything is copied. GreptimeDB updates no row in place and deletes none of an append_mode
// table, so the calls move to a table of their own; a move cut short leaves model_usage, and the
// next start drops the partial model_calls and moves them again.
func (s *Store) moveModelUsage(ctx context.Context, anthropic domain.ModelCatalog) error {
	rows, err := s.querySQLRows(ctx, `SELECT table_name FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'model_usage'`)
	if err != nil || len(rows) == 0 {
		return err
	}
	if err := s.execSQL(ctx, `ALTER TABLE model_usage ADD COLUMN IF NOT EXISTS "cost_usd" DOUBLE NULL`); err != nil {
		return fmt.Errorf("move model_usage: %w", err)
	}
	unpriced, err := s.querySQLRows(ctx, `SELECT DISTINCT model FROM model_usage WHERE cost_usd IS NULL ORDER BY model`)
	if err != nil {
		return fmt.Errorf("move model_usage: %w", err)
	}
	var unknown []string
	price := "CASE model"
	for _, r := range unpriced {
		model, _ := r[0].(string)
		p, ok := anthropic[model]
		if !ok {
			unknown = append(unknown, model)
			continue
		}
		price += fmt.Sprintf(" WHEN %s THEN (input_tokens * %s + output_tokens * %s + cache_creation_input_tokens * %s + cache_read_input_tokens * %s) / 1e6",
			sqlQuote(model), usd(p.InputUSDPerMTok), usd(p.OutputUSDPerMTok), usd(p.CacheWriteUSDPerMTok), usd(p.CacheReadUSDPerMTok))
	}
	if len(unknown) > 0 {
		return fmt.Errorf("move model_usage: it holds calls recorded with no cost of %s, which models.anthropic.catalog does not price: add each with its price", strings.Join(unknown, ", "))
	}
	price += " END"
	if len(unpriced) == 0 {
		price = "NULL"
	}
	columns := "org_id, platform_experiment_id, agent_id, provider, model, input_tokens, output_tokens, cache_creation_input_tokens, cache_read_input_tokens"
	for _, step := range []string{
		`DROP TABLE IF EXISTS model_calls`,
		modelCalls.create,
		`INSERT INTO model_calls (` + columns + `, cost_usd, ts) SELECT ` + columns + `, COALESCE(cost_usd, ` + price + `), ts FROM model_usage`,
		`DROP TABLE model_usage`,
	} {
		if err := s.execSQL(ctx, step); err != nil {
			return fmt.Errorf("move model_usage: %w", err)
		}
	}
	return nil
}

func usd(perMTok float64) string { return strconv.FormatFloat(perMTok, 'g', -1, 64) }
