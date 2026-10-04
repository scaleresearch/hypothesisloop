package metricsdb

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// Each agent's latest health values are read back from its series, the coordinator's under its
// role with no agent, and a first job only for an agent that has one.
func TestLatestRunHealthReadsEachAgentsLatestValues(t *testing.T) {
	answers := map[string]string{
		agentFirstJobSecondsMetric:          `{"metric":{"agent_id":"ag-1","role":"competitor"},"value":[1,"42"]}`,
		agentFirstCompletedJobSecondsMetric: ``,
		agentJobsInFlightMetric:             `{"metric":{"agent_id":"ag-1","role":"competitor"},"value":[1,"2"]},{"metric":{"role":"coordinator"},"value":[1,"0"]}`,
		agentRestartsMetric:                 `{"metric":{"agent_id":"ag-1","role":"competitor"},"value":[1,"3"]},{"metric":{"role":"coordinator"},"value":[1,"1"]}`,
		agentCoordinatorMessagesMetric:      `{"metric":{"agent_id":"ag-1","role":"competitor"},"value":[1,"4"]},{"metric":{"role":"coordinator"},"value":[1,"0"]}`,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query().Get("query")
		if !strings.Contains(query, `{platform_experiment_id="pe-1"}`) {
			t.Errorf("query %s names no platform experiment pe-1", query)
		}
		metric, _, _ := strings.Cut(strings.TrimPrefix(query, "last_over_time("), "{")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[` + answers[metric] + `]}}`))
	}))
	defer server.Close()
	got, err := LatestRunHealth(context.Background(), server.URL, "pe-1", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	firstJob := 42.0
	want := []domain.AgentHealth{
		{PlatformExperimentID: "pe-1", AgentID: "ag-1", Role: "competitor", FirstJobSeconds: &firstJob, JobsInFlight: 2, Restarts: 3, CoordinatorMessages: 4},
		{PlatformExperimentID: "pe-1", Role: "coordinator", Restarts: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}
