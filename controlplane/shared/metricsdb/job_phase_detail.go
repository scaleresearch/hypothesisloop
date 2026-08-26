package metricsdb

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// job_phase_detail follows job_logs.go's pattern exactly: a text field (reason/message), not a
// number series, so it lives in GreptimeDB's own SQL table rather than Prometheus remote-write.
// One row per (experiment_id, ts), latest replaces — a cluster-agent's status push always
// reports its current read of the runtime, not an accumulating history of past ones.

// phase_message, not message: GreptimeDB's SQL parser reserves "message" as a keyword and
// rejects it as a bare column name.
const ensureJobPhaseDetailTableSQL = `CREATE TABLE IF NOT EXISTS job_phase_detail (
	experiment_id STRING,
	cluster_name STRING,
	reason STRING,
	phase_message STRING,
	restart_count BIGINT,
	scheduled_nodes BIGINT,
	scheduling_reason STRING,
	ts TIMESTAMP TIME INDEX,
	PRIMARY KEY(experiment_id)
) WITH(ttl='30d')`

// ensureJobPhaseDetailColumnsSQL adds the autoscaler gang-readiness columns to a job_phase_detail
// table created before they existed. CREATE TABLE IF NOT EXISTS above is a no-op against an
// existing table, so a rolling upgrade needs this explicit ALTER; IF NOT EXISTS makes it safe to
// run on every call alongside the CREATE.
var ensureJobPhaseDetailColumnsSQL = []string{
	`ALTER TABLE job_phase_detail ADD COLUMN IF NOT EXISTS scheduled_nodes BIGINT`,
	`ALTER TABLE job_phase_detail ADD COLUMN IF NOT EXISTS scheduling_reason STRING`,
}

// RecordPhaseDetail stores the runtime's current explanation for experimentID's phase — why its
// container hasn't started, or why it has been restarting — plus the gang-readiness facts the
// scale-up-timeout watcher needs: scheduledNodes (pods with PodScheduled=True) and
// schedulingReason (the autoscaler's own refusal/acceptance signal, best-effort). Replaces
// whatever was previously stored, same "latest snapshot" idiom as RecordLogTail.
func RecordPhaseDetail(ctx context.Context, dbURL, experimentID, clusterName, reason, message string, restartCount int32, scheduledNodes int32, schedulingReason string, at time.Time) error {
	if experimentID == "" {
		return fmt.Errorf("metricsdb.RecordPhaseDetail: experiment_id is required")
	}
	if err := execSQL(ctx, dbURL, ensureJobPhaseDetailTableSQL); err != nil {
		return fmt.Errorf("metricsdb.RecordPhaseDetail: ensure table: %w", err)
	}
	for _, alter := range ensureJobPhaseDetailColumnsSQL {
		if err := execSQL(ctx, dbURL, alter); err != nil {
			return fmt.Errorf("metricsdb.RecordPhaseDetail: ensure columns: %w", err)
		}
	}
	insertSQL := fmt.Sprintf(
		`INSERT INTO job_phase_detail (experiment_id, cluster_name, reason, phase_message, restart_count, scheduled_nodes, scheduling_reason, ts) VALUES (%s, %s, %s, %s, %d, %d, %s, %d)`,
		sqlQuote(experimentID), sqlQuote(clusterName), sqlQuote(reason), sqlQuote(message), restartCount, scheduledNodes, sqlQuote(schedulingReason), at.UnixMilli(),
	)
	if err := execSQL(ctx, dbURL, insertSQL); err != nil {
		return fmt.Errorf("metricsdb.RecordPhaseDetail: insert: %w", err)
	}
	return nil
}

// GetLatestPhaseDetail returns experimentID's most recently reported phase detail. found=false
// (not an error) means nothing has ever been reported for this experiment.
func GetLatestPhaseDetail(ctx context.Context, dbURL, experimentID string) (reason, message string, restartCount int32, found bool, err error) {
	row, found, err := GetLatestPhaseDetailFull(ctx, dbURL, experimentID)
	if err != nil || !found {
		return "", "", 0, found, err
	}
	return row.Reason, row.Message, row.RestartCount, true, nil
}

// GetLatestPhaseDetailFull is GetLatestPhaseDetail plus the gang-readiness columns
// (ScheduledNodes/SchedulingReason) the scale-up-timeout watcher needs. found=false means nothing
// has ever been reported for this experiment.
func GetLatestPhaseDetailFull(ctx context.Context, dbURL, experimentID string) (PhaseDetailRow, bool, error) {
	if experimentID == "" {
		return PhaseDetailRow{}, false, fmt.Errorf("metricsdb.GetLatestPhaseDetailFull: experiment_id is required")
	}
	querySQL := fmt.Sprintf(
		`SELECT reason, phase_message, restart_count, scheduled_nodes, scheduling_reason FROM job_phase_detail WHERE experiment_id = %s ORDER BY ts DESC LIMIT 1`,
		sqlQuote(experimentID),
	)
	rows, err := querySQLRows(ctx, dbURL, querySQL)
	if err != nil {
		// The table not existing yet (nothing has ever been reported for any job) is not an
		// error from the caller's point of view -- same as no rows.
		if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "does not exist") {
			return PhaseDetailRow{}, false, nil
		}
		return PhaseDetailRow{}, false, fmt.Errorf("metricsdb.GetLatestPhaseDetailFull: %w", err)
	}
	if len(rows) == 0 || len(rows[0]) != 5 {
		return PhaseDetailRow{}, false, nil
	}
	return phaseDetailRowFromColumns(rows[0]), true, nil
}

// PhaseDetailRow is one experiment's latest reported phase detail, as returned by
// GetLatestPhaseDetailBatch.
type PhaseDetailRow struct {
	Reason       string
	Message      string
	RestartCount int32
	// ScheduledNodes is the count of this experiment's pods with condition PodScheduled=True, as
	// last reported by the runtime — 0 when never reported (older runtime build) or genuinely
	// zero pods placed yet. Compared against domain.Experiment.Job.Nodes() to detect a partial
	// gang; see the design in autoscaler.md.
	ScheduledNodes int32
	// SchedulingReason is a best-effort, runtime-supplied explanation for why a pod is still
	// Pending — e.g. a CA/Karpenter event message like "TriggeredScaleUp" or "NotTriggerScaleUp:
	// max node group size reached". Empty when the runtime has nothing to report.
	SchedulingReason string
}

func phaseDetailRowFromColumns(row []any) PhaseDetailRow {
	reasonVal, _ := row[0].(string)
	messageVal, _ := row[1].(string)
	restartCountVal := toInt32(row[2])
	scheduledNodesVal := toInt32(row[3])
	schedulingReasonVal, _ := row[4].(string)
	return PhaseDetailRow{
		Reason: reasonVal, Message: messageVal, RestartCount: restartCountVal,
		ScheduledNodes: scheduledNodesVal, SchedulingReason: schedulingReasonVal,
	}
}

func toInt32(v any) int32 {
	switch t := v.(type) {
	case float64:
		return int32(t)
	case int64:
		return int32(t)
	default:
		return 0
	}
}

// GetLatestPhaseDetailBatch returns the latest phase detail for every experiment ID in ids that
// has ever reported one, in a single query — the list-experiments endpoint's equivalent of
// GetLatestPhaseDetail, batched so watching many jobs doesn't cost one query per job. IDs with no
// reported phase detail are simply absent from the result, same as found=false from the
// single-ID lookup.
func GetLatestPhaseDetailBatch(ctx context.Context, dbURL string, ids []string) (map[string]PhaseDetailRow, error) {
	out := make(map[string]PhaseDetailRow)
	if len(ids) == 0 {
		return out, nil
	}
	quoted := make([]string, len(ids))
	for i, id := range ids {
		if id == "" {
			return nil, fmt.Errorf("metricsdb.GetLatestPhaseDetailBatch: experiment_id is required")
		}
		quoted[i] = sqlQuote(id)
	}
	// One row per experiment, chosen in the database. Ordering everything newest-first and taking
	// the first row per id in Go read every sample ever recorded for every job on the page — a
	// list endpoint that got slower the longer the platform had been running, to answer a question
	// whose answer is one row each.
	querySQL := fmt.Sprintf(
		`SELECT experiment_id, reason, phase_message, restart_count, scheduled_nodes, scheduling_reason FROM (`+
			`SELECT experiment_id, reason, phase_message, restart_count, scheduled_nodes, scheduling_reason, `+
			`ROW_NUMBER() OVER (PARTITION BY experiment_id ORDER BY ts DESC) AS rn `+
			`FROM job_phase_detail WHERE experiment_id IN (%s)) WHERE rn = 1`,
		strings.Join(quoted, ", "),
	)
	rows, err := querySQLRows(ctx, dbURL, querySQL)
	if err != nil {
		// The table not existing yet (nothing has ever been reported for any job) is not an
		// error from the caller's point of view -- same as no rows.
		if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "does not exist") {
			return out, nil
		}
		return nil, fmt.Errorf("metricsdb.GetLatestPhaseDetailBatch: %w", err)
	}
	for _, row := range rows {
		if len(row) != 6 {
			return nil, fmt.Errorf("metricsdb.GetLatestPhaseDetailBatch: unexpected row shape")
		}
		id, ok := row[0].(string)
		if !ok || id == "" {
			return nil, fmt.Errorf("metricsdb.GetLatestPhaseDetailBatch: row missing experiment_id")
		}
		if _, seen := out[id]; seen {
			continue
		}
		out[id] = phaseDetailRowFromColumns(row[1:])
	}
	return out, nil
}
