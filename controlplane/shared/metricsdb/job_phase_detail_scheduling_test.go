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

// RecordPhaseDetail must carry scheduled_nodes and scheduling_reason into the INSERT alongside
// reason/message/restart_count -- these are the gang-readiness facts autoscaler.md's watcher
// reads to decide whether a partial gang is still within its scale-up deadline.
func TestRecordPhaseDetailInsertsScheduledNodesAndSchedulingReason(t *testing.T) {
	var sawInsert bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		sql := r.Form.Get("sql")
		if strings.HasPrefix(sql, "INSERT") {
			sawInsert = true
			if !strings.Contains(sql, "scheduled_nodes") || !strings.Contains(sql, "scheduling_reason") {
				t.Errorf("INSERT missing scheduled_nodes/scheduling_reason columns: %s", sql)
			}
			if !strings.Contains(sql, "2") || !strings.Contains(sql, "TriggeredScaleUp") {
				t.Errorf("INSERT does not carry the reported values: %s", sql)
			}
		}
		_, _ = fmt.Fprint(w, `{"output":[{"records":{"schema":{"column_schemas":[]},"rows":[]}}]}`)
	}))
	defer server.Close()

	err := RecordPhaseDetail(context.Background(), server.URL, "exp-1", "cluster-a", "", "", 0, 2, "TriggeredScaleUp", time.Now())
	if err != nil {
		t.Fatalf("RecordPhaseDetail: %v", err)
	}
	if !sawInsert {
		t.Fatal("no INSERT statement was sent")
	}
}

// GetLatestPhaseDetailFull must decode the two new columns back into PhaseDetailRow, in addition
// to the pre-existing reason/message/restart_count fields.
func TestGetLatestPhaseDetailFullDecodesSchedulingColumns(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"output":[{"records":{"schema":{"column_schemas":[{"name":"reason"},{"name":"phase_message"},{"name":"restart_count"},{"name":"scheduled_nodes"},{"name":"scheduling_reason"}]},"rows":[["",  "waiting", 1, 2, "TriggeredScaleUp"]]}}]}`)
	}))
	defer server.Close()

	row, found, err := GetLatestPhaseDetailFull(context.Background(), server.URL, "exp-1")
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
