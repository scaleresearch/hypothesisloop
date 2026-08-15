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
	ts TIMESTAMP TIME INDEX,
	PRIMARY KEY(experiment_id)
)`

// RecordPhaseDetail stores the runtime's current explanation for experimentID's phase — why its
// container hasn't started, or why it has been restarting. Replaces whatever was previously
// stored, same "latest snapshot" idiom as RecordLogTail.
func RecordPhaseDetail(ctx context.Context, dbURL, experimentID, clusterName, reason, message string, restartCount int32, at time.Time) error {
	if experimentID == "" {
		return fmt.Errorf("metricsdb.RecordPhaseDetail: experiment_id is required")
	}
	if err := execSQL(ctx, dbURL, ensureJobPhaseDetailTableSQL); err != nil {
		return fmt.Errorf("metricsdb.RecordPhaseDetail: ensure table: %w", err)
	}
	insertSQL := fmt.Sprintf(
		`INSERT INTO job_phase_detail (experiment_id, cluster_name, reason, phase_message, restart_count, ts) VALUES (%s, %s, %s, %s, %d, %d)`,
		sqlQuote(experimentID), sqlQuote(clusterName), sqlQuote(reason), sqlQuote(message), restartCount, at.UnixMilli(),
	)
	if err := execSQL(ctx, dbURL, insertSQL); err != nil {
		return fmt.Errorf("metricsdb.RecordPhaseDetail: insert: %w", err)
	}
	return nil
}

// GetLatestPhaseDetail returns experimentID's most recently reported phase detail. found=false
// (not an error) means nothing has ever been reported for this experiment.
func GetLatestPhaseDetail(ctx context.Context, dbURL, experimentID string) (reason, message string, restartCount int32, found bool, err error) {
	if experimentID == "" {
		return "", "", 0, false, fmt.Errorf("metricsdb.GetLatestPhaseDetail: experiment_id is required")
	}
	querySQL := fmt.Sprintf(
		`SELECT reason, phase_message, restart_count FROM job_phase_detail WHERE experiment_id = %s ORDER BY ts DESC LIMIT 1`,
		sqlQuote(experimentID),
	)
	rows, err := querySQLRows(ctx, dbURL, querySQL)
	if err != nil {
		// The table not existing yet (nothing has ever been reported for any job) is not an
		// error from the caller's point of view -- same as no rows.
		if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "does not exist") {
			return "", "", 0, false, nil
		}
		return "", "", 0, false, fmt.Errorf("metricsdb.GetLatestPhaseDetail: %w", err)
	}
	if len(rows) == 0 || len(rows[0]) != 3 {
		return "", "", 0, false, nil
	}
	reasonVal, _ := rows[0][0].(string)
	messageVal, _ := rows[0][1].(string)
	var restartCountVal int32
	switch v := rows[0][2].(type) {
	case float64:
		restartCountVal = int32(v)
	case int64:
		restartCountVal = int32(v)
	}
	return reasonVal, messageVal, restartCountVal, true, nil
}

// PhaseDetailRow is one experiment's latest reported phase detail, as returned by
// GetLatestPhaseDetailBatch.
type PhaseDetailRow struct {
	Reason       string
	Message      string
	RestartCount int32
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
	querySQL := fmt.Sprintf(
		`SELECT experiment_id, reason, phase_message, restart_count FROM job_phase_detail WHERE experiment_id IN (%s) ORDER BY ts DESC`,
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
	// Rows are ordered newest-first across all requested IDs; the first row seen for a given
	// experiment_id is therefore its latest — later rows for the same ID are older and skipped.
	for _, row := range rows {
		if len(row) != 4 {
			return nil, fmt.Errorf("metricsdb.GetLatestPhaseDetailBatch: unexpected row shape")
		}
		id, ok := row[0].(string)
		if !ok || id == "" {
			return nil, fmt.Errorf("metricsdb.GetLatestPhaseDetailBatch: row missing experiment_id")
		}
		if _, seen := out[id]; seen {
			continue
		}
		reasonVal, _ := row[1].(string)
		messageVal, _ := row[2].(string)
		var restartCountVal int32
		switch v := row[3].(type) {
		case float64:
			restartCountVal = int32(v)
		case int64:
			restartCountVal = int32(v)
		}
		out[id] = PhaseDetailRow{Reason: reasonVal, Message: messageVal, RestartCount: restartCountVal}
	}
	return out, nil
}
