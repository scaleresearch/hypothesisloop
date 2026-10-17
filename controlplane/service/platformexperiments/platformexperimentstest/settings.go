package platformexperimentstest

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// Settings is the quota settings of a deployment that runs one stage over a whole run.
var Settings = domain.QuotaConfig{
	DefaultStages: []domain.Stage{{LengthPct: 100}}, DefaultReportIntervalSeconds: 30,
	DefaultQuotaTier: domain.QuotaTierGuaranteed, MaxRunLength: 30 * 24 * time.Hour, BaselineLimit: 90 * time.Minute, MinBaseline: 20 * time.Minute,
}

// NoUsage is the address of a metrics store that answers every query with no usage.
func NoUsage(t *testing.T) string {
	metrics := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	t.Cleanup(metrics.Close)
	return metrics.URL
}
