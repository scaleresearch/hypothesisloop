package metricsdb

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// GetLatestPhaseDetailFull must decode the two new columns back into PhaseDetailRow, in addition
// to the pre-existing reason/message/restart_count fields.
func TestGetLatestPhaseDetailFullDecodesSchedulingColumns(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"output":[{"records":{"schema":{"column_schemas":[{"name":"reason"},{"name":"phase_message"},{"name":"restart_count"},{"name":"scheduled_nodes"},{"name":"scheduling_reason"}]},"rows":[["",  "waiting", 1, 2, "TriggeredScaleUp"]]}}]}`)
	}))
	defer server.Close()

	row, found, err := New(server.URL).GetLatestPhaseDetailFull(context.Background(), "exp-1", "cluster-a", 0)
	if err != nil {
		t.Fatalf("GetLatestPhaseDetailFull: %v", err)
	}
	if !found {
		t.Fatal("found = false, want true")
	}
	if row.ScheduledNodes != 2 {
		t.Errorf("ScheduledNodes = %d, want 2", row.ScheduledNodes)
	}
	if row.SchedulingReason != "TriggeredScaleUp" {
		t.Errorf("SchedulingReason = %q, want TriggeredScaleUp", row.SchedulingReason)
	}
	if row.Message != "waiting" || row.RestartCount != 1 {
		t.Errorf("pre-existing fields regressed: message=%q restart_count=%d", row.Message, row.RestartCount)
	}
}

// PhaseReportedSince asks for the first row of one attempt under one reason, and an attempt that
// never reported the reason, which the store answers with a null, is not found.
func TestPhaseReportedSinceIsTheFirstReportOfTheReasonInTheAttempt(t *testing.T) {
	answer := `[[1791288271695]]`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		sql := r.Form.Get("sql")
		for _, want := range []string{"MIN(ts)", "experiment_id = 'exp-1'", "cluster_name = 'cluster-a'", "attempt = 2", "reason = 'instance_starting'"} {
			if !strings.Contains(sql, want) {
				t.Errorf("query lacks %q: %s", want, sql)
			}
		}
		_, _ = fmt.Fprintf(w, `{"output":[{"records":{"schema":{"column_schemas":[{"name":"min(job_phase_detail.ts)"}]},"rows":%s}}]}`, answer)
	}))
	defer server.Close()

	since, found, err := New(server.URL).PhaseReportedSince(context.Background(), "exp-1", "cluster-a", 2, "instance_starting")
	if err != nil || !found || !since.Equal(time.UnixMilli(1791288271695)) {
		t.Fatalf("since = %v, found = %v, err = %v, want the first report's time", since, found, err)
	}
	answer = `[[null]]`
	if _, found, err := New(server.URL).PhaseReportedSince(context.Background(), "exp-1", "cluster-a", 2, "instance_starting"); err != nil || found {
		t.Fatalf("found = %v, err = %v, want not found for a reason never reported", found, err)
	}
}
