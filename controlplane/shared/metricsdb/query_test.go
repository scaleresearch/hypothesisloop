package metricsdb

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func queryServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}

func TestQueryVectorRejectsMalformedSampleInsteadOfReturningPartialState(t *testing.T) {
	server := queryServer(t, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"cluster_name":"a"},"value":[1,"2"]},{"metric":{"cluster_name":"b"},"value":[1,"broken"]}]}}`)
	defer server.Close()

	_, err := QueryVector(context.Background(), server.URL, "up")
	if err == nil || !strings.Contains(err.Error(), "result 1") {
		t.Fatalf("QueryVector error = %v, want malformed second sample error", err)
	}
}

func TestQueryVectorRejectsPrometheusErrorEnvelope(t *testing.T) {
	server := queryServer(t, `{"status":"error","errorType":"execution","error":"bad query"}`)
	defer server.Close()

	_, err := QueryVector(context.Background(), server.URL, "up")
	if err == nil || !strings.Contains(err.Error(), "bad query") {
		t.Fatalf("QueryVector error = %v, want Prometheus error", err)
	}
}

func TestQueryRangeRejectsMalformedPointInsteadOfShorteningActualHistory(t *testing.T) {
	server := queryServer(t, `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"experiment_id":"e"},"values":[[1,"1"],[2,"NaN"]]}]}}`)
	defer server.Close()

	_, err := QueryRange(context.Background(), server.URL, "up", time.Unix(0, 0), time.Unix(3, 0), time.Second)
	if err == nil || !strings.Contains(err.Error(), "point 1") {
		t.Fatalf("QueryRange error = %v, want malformed second point error", err)
	}
}
