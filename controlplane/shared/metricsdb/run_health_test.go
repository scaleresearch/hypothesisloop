package metricsdb

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gogo/protobuf/proto"
	"github.com/golang/snappy"

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

// A hosted agent's rate limit is read back beside its other health values: its utilization, and
// while the provider refuses its calls the reset it waits for. A refusal whose reset has passed is
// over even before a reading says so.
func TestLatestRunHealthReadsWhetherAnAgentIsRateLimited(t *testing.T) {
	future, past := time.Now().Add(48*time.Hour).Truncate(time.Second).UTC(), time.Now().Add(-time.Minute).Unix()
	answers := map[string]string{
		agentRateLimitUtilizationMetric: `{"metric":{"agent_id":"ag-1","role":"competitor"},"value":[1,"0.95"]}`,
		agentRateLimitRejectedMetric:    `{"metric":{"agent_id":"ag-1","role":"competitor"},"value":[1,"1"]},{"metric":{"role":"coordinator"},"value":[1,"1"]}`,
		agentRateLimitResetsAtMetric: fmt.Sprintf(`{"metric":{"agent_id":"ag-1","role":"competitor"},"value":[1,"%d"]},{"metric":{"role":"coordinator"},"value":[1,"%d"]}`,
			future.Unix(), past),
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		metric, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Query().Get("query"), "last_over_time("), "{")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[` + answers[metric] + `]}}`))
	}))
	defer server.Close()
	got, err := LatestRunHealth(context.Background(), server.URL, "pe-1", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	utilization := 0.95
	want := []domain.AgentHealth{
		{PlatformExperimentID: "pe-1", AgentID: "ag-1", Role: "competitor", RateLimitUtilization: &utilization, RateLimitedUntil: &future},
		{PlatformExperimentID: "pe-1", Role: "coordinator"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// What a harness last said of itself is read back per agent and for the coordinator: subagents,
// turn and when it began, when it was read, and the rate limit, whose refusal is over once its
// reset has passed. A series with no state line of its own is left out.
func TestLatestHarnessStateReadsWhatEachHarnessLastSaid(t *testing.T) {
	future, past := time.Now().Add(48*time.Hour).Truncate(time.Second).UTC(), time.Now().Add(-time.Minute).Truncate(time.Second).UTC()
	answers := map[string]string{
		agentStateReportedAtMetric:      `{"metric":{"agent_id":"ag-1","role":"competitor"},"value":[1,"1760000100"]},{"metric":{"role":"coordinator"},"value":[1,"1760000200"]}`,
		agentSubagentsMetric:            `{"metric":{"agent_id":"ag-1","role":"competitor"},"value":[1,"3"]},{"metric":{"role":"coordinator"},"value":[1,"0"]}`,
		agentTurnWaitingMetric:          `{"metric":{"agent_id":"ag-1","role":"competitor"},"value":[1,"0"]},{"metric":{"role":"coordinator"},"value":[1,"1"]}`,
		agentInstructionsDigestMetric:   `{"metric":{"agent_id":"ag-1","role":"competitor"},"value":[1,"4886718345"]}`,
		agentTurnSinceMetric:            `{"metric":{"agent_id":"ag-1","role":"competitor"},"value":[1,"1760000000"]},{"metric":{"role":"coordinator"},"value":[1,"1760000050"]}`,
		agentRateLimitUtilizationMetric: `{"metric":{"agent_id":"ag-1","role":"competitor"},"value":[1,"0.95"]},{"metric":{"agent_id":"ag-2","role":"competitor"},"value":[1,"0.5"]}`,
		agentRateLimitRejectedMetric:    `{"metric":{"agent_id":"ag-1","role":"competitor"},"value":[1,"1"]},{"metric":{"role":"coordinator"},"value":[1,"1"]}`,
		agentRateLimitResetsAtMetric: fmt.Sprintf(`{"metric":{"agent_id":"ag-1","role":"competitor"},"value":[1,"%d"]},{"metric":{"role":"coordinator"},"value":[1,"%d"]}`,
			future.Unix(), past.Unix()),
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
	got, err := LatestHarnessState(context.Background(), server.URL, "pe-1", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	utilization := 0.95
	want := []domain.HarnessState{
		{PlatformExperimentID: "pe-1", AgentID: "ag-1", Role: "competitor", Subagents: 3, TurnSince: time.Unix(1760000000, 0).UTC(), InstructionsDigest: "000123456789",
			Model: domain.RateLimit{Rejected: true, Utilization: &utilization, ResetsAt: &future}, At: time.Unix(1760000100, 0).UTC()},
		{PlatformExperimentID: "pe-1", Role: "coordinator", TurnWaiting: true, TurnSince: time.Unix(1760000050, 0).UTC(),
			Model: domain.RateLimit{ResetsAt: &past}, At: time.Unix(1760000200, 0).UTC()},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// Each state is written as its gauges, the rate-limit ones only of a harness that has read one, and
// the version of its agent type, labelled with the type, only of one that said it.
func TestRecordHarnessStateWritesTheRateLimitOnlyOnceRead(t *testing.T) {
	written := map[string][]string{}
	versionOf := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := snappy.Decode(nil, raw)
		if err != nil {
			t.Fatal(err)
		}
		var req rwWriteRequest
		if err := proto.Unmarshal(decoded, &req); err != nil {
			t.Fatal(err)
		}
		for _, ts := range req.Timeseries {
			name, role, agentType := "", "", ""
			for _, l := range ts.Labels {
				switch l.Name {
				case "__name__":
					name = l.Value
				case "role":
					role = l.Value
				case "agent_type":
					agentType = l.Value
				}
			}
			if name == agentInstructionsVersionMetric {
				versionOf = agentType
			}
			written[role] = append(written[role], name)
		}
	}))
	defer server.Close()
	utilization := 0.4
	err := RecordHarnessState(context.Background(), server.URL, time.Now(), []domain.HarnessState{
		{PlatformExperimentID: "pe-1", AgentID: "ag-1", Role: "competitor", Subagents: 2, TurnSince: time.Now(), Model: domain.RateLimit{Utilization: &utilization}},
		{PlatformExperimentID: "pe-1", Role: "coordinator", TurnWaiting: true, TurnSince: time.Now(), InstructionsDigest: "0123456789ab", AgentType: "coordinator", AgentTypeVersion: 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	state := []string{agentStateReportedAtMetric, agentSubagentsMetric, agentTurnWaitingMetric, agentTurnSinceMetric}
	want := map[string][]string{
		"competitor":  append(slices.Clone(state), agentRateLimitRejectedMetric, agentRateLimitUtilizationMetric),
		"coordinator": append(slices.Clone(state), agentInstructionsDigestMetric, agentInstructionsVersionMetric),
	}
	if !reflect.DeepEqual(written, want) || versionOf != "coordinator" {
		t.Errorf("written %v of version labelled %q, want %v of coordinator", written, versionOf, want)
	}
}
