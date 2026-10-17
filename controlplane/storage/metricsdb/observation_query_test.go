package metricsdb

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// spreadServer answers the sample count with two points and each spread query with the spread
// given for its series.
func spreadServer(t *testing.T, valueSpread, fractionSpread string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		query := r.Form.Get("query")
		value := "2"
		switch {
		case strings.HasPrefix(query, "count_over_time("):
		case strings.Contains(query, ExperimentMetricFraction):
			value = fractionSpread
		default:
			value = valueSpread
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"` + value + `"]}]}}`))
	}))
}

// A metric that plateaus while fraction_complete still moves is a job making progress. Only one
// whose value and fraction both hold reads as stuck.
func TestAnyDeclaredMetricChangedCountsAMovingFractionAsProgress(t *testing.T) {
	for _, tc := range []struct {
		value, fraction string
		changed         bool
	}{
		{"0", "0.05", true},
		{"0.1", "0", true},
		{"0", "0", false},
	} {
		server := spreadServer(t, tc.value, tc.fraction)
		reported, changed, err := New(server.URL).AnyDeclaredMetricChanged(context.Background(), "job-1", []string{"loss"}, time.Hour)
		server.Close()
		if err != nil {
			t.Fatalf("value spread %s, fraction spread %s: %v", tc.value, tc.fraction, err)
		}
		if !reported || changed != tc.changed {
			t.Errorf("value spread %s, fraction spread %s: reported, changed = %v, %v, want true, %v", tc.value, tc.fraction, reported, changed, tc.changed)
		}
	}
}
