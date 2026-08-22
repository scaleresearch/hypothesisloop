package metricsdb

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Every job-scoped observation query scans from the job's own creation, so a job older than any
// fixed horizon still measures its whole life. The horizons this replaced disagreed — settlement
// scanned the full lifetime while the controller and the scheduler capped at 14 days — which made
// the same job cost one number to the code that bills it and a smaller one to the code that
// evicts it.
func TestJobScopedQueriesScanFromCreationHoweverOldTheJobIs(t *testing.T) {
	createdAt := time.Now().UTC().Add(-60 * 24 * time.Hour)
	now := time.Now().UTC()

	var sql string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sql = r.URL.Query().Get("sql")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output":[{"records":{"rows":[[null,null,null,null]]}}]}`))
	}))
	defer server.Close()

	if _, err := ObservedElapsedHours(context.Background(), server.URL, "exp-1", createdAt, now, time.Minute); err != nil {
		t.Fatalf("ObservedElapsedHours: %v", err)
	}

	want := ObservationWindowStart(createdAt).UnixMilli()
	if !strings.Contains(sql, strconv.FormatInt(want, 10)) {
		t.Fatalf("query did not scan from the job's creation (%d):\n%s", want, sql)
	}
}

// created_at is stamped by Postgres and observations by cluster clocks. Without a margin, a
// cluster running slightly behind loses its earliest samples from every measurement — silent
// underbilling no query can detect, because the samples fall outside the window that was asked for.
func TestObservationWindowStartsBeforeCreationBySkewMargin(t *testing.T) {
	createdAt := time.Now().UTC()
	if got := createdAt.Sub(ObservationWindowStart(createdAt)); got != MaxObservationClockSkew {
		t.Fatalf("window starts %v before creation, want %v", got, MaxObservationClockSkew)
	}
}
