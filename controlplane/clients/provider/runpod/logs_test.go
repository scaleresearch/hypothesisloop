package runpod

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

// stream answers a log request with the backfill at once, then keeps the stream open, sending
// live after a while when it is set.
func stream(backfill []string, at time.Time, live string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for n, line := range backfill {
			ts := at.Add(time.Duration(n) * time.Millisecond).Format(time.RFC3339Nano)
			fmt.Fprintf(w, "id: %s\ndata: {\"source\":\"container\",\"line\":%q,\"ts\":%q}\n\n", ts, line, ts)
		}
		w.(http.Flusher).Flush()
		if live != "" {
			time.Sleep(20 * time.Millisecond)
			ts := time.Now().Add(time.Hour).Format(time.RFC3339Nano)
			fmt.Fprintf(w, "data: {\"source\":\"container\",\"line\":%q,\"ts\":%q}\n\n", live, ts)
			w.(http.Flusher).Flush()
		}
		<-r.Context().Done()
	}
}

func TestLogsReadTheBackfillOfTheContainersOutput(t *testing.T) {
	api := newFakeAPI(t)
	p := newTestProvider(t, api, testSettings())
	ctx := context.Background()
	past := time.Now().Add(-time.Minute)
	api.on(http.MethodGet, "/v2/pods/p1/logs", stream([]string{"one", `two "quoted" \ kept`, "three"}, past, ""))
	lines, err := p.Logs(ctx, "p1", 2)
	if err != nil || !reflect.DeepEqual(lines, []string{`two "quoted" \ kept`, "three"}) {
		t.Fatalf("Logs = %q, %v", lines, err)
	}
	q := api.sent(http.MethodGet, "/v2/pods/p1/logs")[0].Query
	if !strings.Contains(q, "source=container") || !strings.Contains(q, "tail=2") {
		t.Errorf("query %q", q)
	}

	started := time.Now()
	api.on(http.MethodGet, "/v2/pods/p1/logs", stream([]string{"old"}, past, "new"))
	lines, err = p.Logs(ctx, "p1", 10)
	if err != nil || !reflect.DeepEqual(lines, []string{"old"}) {
		t.Fatalf("with a line written after the read: Logs = %q, %v", lines, err)
	}
	if time.Since(started) > p.logQuiet {
		t.Errorf("a live line did not end the read: it took %s", time.Since(started))
	}

	api.on(http.MethodGet, "/v2/pods/p1/logs", stream(nil, past, ""))
	if lines, err := p.Logs(ctx, "p1", 10); err != nil || len(lines) != 0 || lines == nil {
		t.Fatalf("no output yet: Logs = %#v, %v", lines, err)
	}
	if _, err := p.Logs(ctx, "p1", 9000); err != nil {
		t.Fatal(err)
	}
	if q := api.sent(http.MethodGet, "/v2/pods/p1/logs"); !strings.Contains(q[len(q)-1].Query, "tail=5000") {
		t.Errorf("a tail above the API's most was sent as %q", q[len(q)-1].Query)
	}

	api.on(http.MethodGet, "/v2/pods/gone/logs", problem(404, "pod not found"))
	if _, err := p.Logs(ctx, "gone", 10); err == nil || !strings.Contains(err.Error(), "pod not found") {
		t.Fatalf("logs of a pod that is gone: %v", err)
	}
	if _, err := p.Logs(ctx, "p1", 0); err == nil {
		t.Fatal("a tail of 0 was accepted")
	}
}
