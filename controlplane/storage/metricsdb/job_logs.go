package metricsdb

import (
	"context"
	"encoding/json"
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
// One row per (experiment_id, ts): each report from a cluster-agent (see clusterexchange) is a
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
func (s *Store) RecordLogTail(ctx context.Context, experimentID, clusterName string, lines []string, at time.Time) error {
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
	insertSQL := fmt.Sprintf(
		`INSERT INTO job_logs (experiment_id, cluster_name, lines_json, ts) VALUES (%s, %s, %s, %d)`,
		sqlQuote(experimentID), sqlQuote(clusterName), sqlQuote(string(linesJSON)), at.UnixMilli(),
	)
	if err := s.execSQL(ctx, insertSQL); err != nil {
		return fmt.Errorf("metricsdb.RecordLogTail: insert: %w", err)
	}
	return nil
}

// GetLatestLogTail returns up to the last n lines of experimentID's most recently reported log
// tail (n<=0 returns everything in that latest report). Empty, not an error, if nothing has
// been reported yet.
func (s *Store) GetLatestLogTail(ctx context.Context, experimentID string, n int) ([]string, error) {
	if experimentID == "" {
		return nil, fmt.Errorf("metricsdb.GetLatestLogTail: experiment_id is required")
	}
	querySQL := fmt.Sprintf(
		`SELECT lines_json FROM job_logs WHERE experiment_id = %s ORDER BY ts DESC LIMIT 1`,
		sqlQuote(experimentID),
	)
	rows, err := s.querySQLRows(ctx, querySQL)
	if err != nil {
		return nil, fmt.Errorf("metricsdb.GetLatestLogTail: %w", err)
	}
	if len(rows) == 0 || len(rows[0]) == 0 {
		return []string{}, nil
	}
	linesJSON, ok := rows[0][0].(string)
	if !ok {
		return nil, fmt.Errorf("metricsdb.GetLatestLogTail: unexpected lines_json type %T", rows[0][0])
	}
	lines := []string{}
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

// execSQL runs a SQL statement against GreptimeDB with no result rows expected (DDL/DML).
func (s *Store) execSQL(ctx context.Context, sql string) error {
	_, err := s.doSQL(ctx, sql)
	return err
}

// querySQLRows runs a SQL query and returns its result rows. Every table it reads is created at
// boot (EnsureObservationTables), so one that is missing is an error.
func (s *Store) querySQLRows(ctx context.Context, sql string) ([][]any, error) {
	resp, err := s.doSQL(ctx, sql)
	if err != nil {
		return nil, err
	}
	if len(resp.Output) == 0 || resp.Output[0].Records == nil {
		return nil, nil
	}
	return resp.Output[0].Records.Rows, nil
}

func (s *Store) doSQL(ctx context.Context, sql string) (*greptimeSQLResponse, error) {
	form := url.Values{"sql": {sql}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url+"/v1/sql", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := httpClient.Do(req)
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
		return nil, fmt.Errorf("greptimedb: %s", out.Error)
	}
	return &out, nil
}
