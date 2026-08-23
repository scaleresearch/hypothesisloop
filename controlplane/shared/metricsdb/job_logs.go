package metricsdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// job_logs is a log tail, not a number series, so it can't travel over Prometheus remote-write
// (values there are float64) — this talks to GreptimeDB's own SQL HTTP endpoint instead. Same
// database, same package, still the only place log content lives: no Postgres duplicate.
//
// One row per (experiment_id, ts): each report from a cluster-agent (see clusteragentapi) is a
// full replacement snapshot of "this job's current tail", not an appended line — reading back
// always takes the single latest row for an experiment_id, same "latest snapshot" idiom as
// LatestJobPhase/LatestExperimentNode elsewhere in this package.

const ensureJobLogsTableSQL = `CREATE TABLE IF NOT EXISTS job_logs (
	experiment_id STRING,
	cluster_name STRING,
	lines_json STRING,
	ts TIMESTAMP TIME INDEX,
	PRIMARY KEY(experiment_id)
) WITH(ttl='30d')`

// RecordLogTail stores lines as the current log tail for experimentID, reported by a
// cluster-agent for clusterName at time at. Replaces whatever was previously stored for this
// experiment — not an accumulating archive.
func RecordLogTail(ctx context.Context, dbURL, experimentID, clusterName string, lines []string, at time.Time) error {
	if experimentID == "" {
		return fmt.Errorf("metricsdb.RecordLogTail: experiment_id is required")
	}
	if lines == nil {
		lines = []string{}
	}
	linesJSON, err := json.Marshal(lines)
	if err != nil {
		return fmt.Errorf("metricsdb.RecordLogTail: encode lines: %w", err)
	}
	if err := execSQL(ctx, dbURL, ensureJobLogsTableSQL); err != nil {
		return fmt.Errorf("metricsdb.RecordLogTail: ensure table: %w", err)
	}
	insertSQL := fmt.Sprintf(
		`INSERT INTO job_logs (experiment_id, cluster_name, lines_json, ts) VALUES (%s, %s, %s, %d)`,
		sqlQuote(experimentID), sqlQuote(clusterName), sqlQuote(string(linesJSON)), at.UnixMilli(),
	)
	if err := execSQL(ctx, dbURL, insertSQL); err != nil {
		return fmt.Errorf("metricsdb.RecordLogTail: insert: %w", err)
	}
	return nil
}

// GetLatestLogTail returns up to the last n lines of experimentID's most recently reported log
// tail (n<=0 returns everything in that latest report). Empty, not an error, if nothing has
// been reported yet.
func GetLatestLogTail(ctx context.Context, dbURL, experimentID string, n int) ([]string, error) {
	if experimentID == "" {
		return nil, fmt.Errorf("metricsdb.GetLatestLogTail: experiment_id is required")
	}
	querySQL := fmt.Sprintf(
		`SELECT lines_json FROM job_logs WHERE experiment_id = %s ORDER BY ts DESC LIMIT 1`,
		sqlQuote(experimentID),
	)
	rows, err := querySQLRows(ctx, dbURL, querySQL)
	if err != nil {
		// The table not existing yet (nothing has ever been reported for any job) is not an
		// error from the caller's point of view -- same as no rows.
		if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "does not exist") {
			return []string{}, nil
		}
		return nil, fmt.Errorf("metricsdb.GetLatestLogTail: %w", err)
	}
	if len(rows) == 0 || len(rows[0]) == 0 {
		return []string{}, nil
	}
	linesJSON, ok := rows[0][0].(string)
	if !ok {
		return nil, fmt.Errorf("metricsdb.GetLatestLogTail: unexpected lines_json type %T", rows[0][0])
	}
	var lines []string
	if err := json.Unmarshal([]byte(linesJSON), &lines); err != nil {
		return nil, fmt.Errorf("metricsdb.GetLatestLogTail: decode lines: %w", err)
	}
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}

// sqlQuote escapes s for embedding as a SQL string literal (single quotes doubled, GreptimeDB's
// SQL dialect is standard-SQL here). There is no parameterized-query endpoint over GreptimeDB's
// HTTP SQL API, so this is the only way to pass a value.
func sqlQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

type greptimeSQLResponse struct {
	Output []struct {
		Records *struct {
			Rows [][]any `json:"rows"`
		} `json:"records"`
	} `json:"output"`
	Error string `json:"error"`
	Code  int    `json:"code"`
}

// greptimeCodeTableNotFound is GreptimeDB's typed code for a query naming a table that does not
// exist. Matched on the code, never on the message text, for the same reason the fault taxonomy
// is: a message is free to be reworded and a code is not.
const greptimeCodeTableNotFound = 4001

// errTableNotFound lets querySQLRows recognise that specific failure without re-inspecting the
// response, and lets any other caller opt in explicitly rather than by accident.
var errTableNotFound = errors.New("metricsdb: table does not exist yet")

// execSQL runs a SQL statement against GreptimeDB with no result rows expected (DDL/DML).
func execSQL(ctx context.Context, dbURL, sql string) error {
	_, err := doSQL(ctx, dbURL, sql)
	return err
}

// querySQLRows runs a SQL query and returns its result rows.
//
// A table that does not exist yet reads as no rows, not as a failure. Metric tables are created
// by the first write to them, so on a deployment where no job has reported yet EVERY read of one
// is a table-not-found — and treating that as an error meant settlement could not compute
// observed hours, so no job's quota was ever refunded until some unrelated job happened to post
// the first metric. "Nothing has been written" and "this job wrote nothing" are the same answer,
// and it is zero rows.
//
// Deliberately not applied to execSQL: a DDL statement naming a missing table is a real fault.
func querySQLRows(ctx context.Context, dbURL, sql string) ([][]any, error) {
	resp, err := doSQL(ctx, dbURL, sql)
	if err != nil {
		if errors.Is(err, errTableNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if len(resp.Output) == 0 || resp.Output[0].Records == nil {
		return nil, nil
	}
	return resp.Output[0].Records.Rows, nil
}

func doSQL(ctx context.Context, dbURL, sql string) (*greptimeSQLResponse, error) {
	form := url.Values{"sql": {sql}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, dbURL+"/v1/sql", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	var out greptimeSQLResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode response: %w (body=%s)", err, string(body))
	}
	if out.Error != "" {
		if out.Code == greptimeCodeTableNotFound {
			return nil, fmt.Errorf("greptimedb: %s: %w", out.Error, errTableNotFound)
		}
		return nil, fmt.Errorf("greptimedb: %s", out.Error)
	}
	return &out, nil
}
