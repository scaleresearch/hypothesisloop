package registry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// oneExperimentStore answers GetExperiment with a single fixed row — the only thing GetTimeseries
// asks persistence for, since it sizes its query window off the real experiment's lifetime.
type oneExperimentStore struct {
	Store
	exp *domain.Experiment
}

func (s oneExperimentStore) GetExperiment(_ context.Context, _ string) (*domain.Experiment, error) {
	return s.exp, nil
}

// recordedSamplesServer plays a metrics store holding three samples of one metric taken a
// fraction of a second apart — what a three-node job reporting its own rank actually looks like,
// since a rank is not a label and every node writes into one series. It answers the SQL read with
// those samples as stored, and a PromQL range query the way a real store does: on the grid, where
// only the last sample inside a step survives and is then carried forward.
func recordedSamplesServer(at time.Time) *httptest.Server {
	ms := at.UnixMilli()
	row := func(value string, offsetMillis int64) string {
		return `["agent-1",` + strconv.FormatInt(ms+offsetMillis, 10) + `,` + value + `,"exp-1","raw","stage_rank","pe-1"]`
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "query_range") {
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[` +
				`{"metric":{"job_id":"exp-1","agent_id":"agent-1","metric_name":"stage_rank","metric_basis":"raw"},` +
				`"values":[[` + strconv.FormatInt((ms+5000)/1000, 10) + `,"2"]]}]}}`))
			return
		}
		// Each node posts its value and the fraction it was at, at one instant. All three are at
		// the end of their work, so all three fractions are 1.
		rows := row("0", 0) + `,` + row("1", 400) + `,` + row("2", 420)
		if strings.Contains(r.URL.Query().Get("sql"), "experiment_metric_fraction") {
			rows = row("1", 0) + `,` + row("1", 400) + `,` + row("1", 420)
		}
		_, _ = w.Write([]byte(`{"output":[{"records":{"schema":{"column_schemas":[
			{"name":"agent_id","data_type":"String"},
			{"name":"greptime_timestamp","data_type":"TimestampMillisecond"},
			{"name":"greptime_value","data_type":"Float64"},
			{"name":"job_id","data_type":"String"},
			{"name":"metric_basis","data_type":"String"},
			{"name":"metric_name","data_type":"String"},
			{"name":"platform_experiment_id","data_type":"String"}]},
			"rows":[` + rows + `]}}]}`))
	}))
}

// Every node of a distributed job reports under the same label set, because a rank is not a
// label — so three ranks posting three different values land in one series milliseconds apart.
// Reading that series back with a range query evaluated it on a grid and reported, at each grid
// point, the last sample at or before it: two of the three values never appeared at all and the
// survivor was repeated for the length of the run. The samples were in the store the whole time;
// only the read threw them away, which made a correctly ranked three-node job read exactly like
// three pods all claiming one rank.
func TestGetTimeseriesReportsEveryNodesSampleWhenTheyLandInsideOneStep(t *testing.T) {
	at := time.Now().UTC().Add(-time.Minute)
	server := recordedSamplesServer(at)
	defer server.Close()

	svc := &Service{
		store:        oneExperimentStore{exp: &domain.Experiment{ID: "exp-1", AgentID: "agent-1", CreatedAt: at.Add(-time.Hour)}},
		metricsDBURL: server.URL,
	}
	points, err := svc.GetTimeseries(context.Background(), "exp-1")
	if err != nil {
		t.Fatalf("GetTimeseries: %v", err)
	}
	distinct := map[float64]bool{}
	for _, p := range points {
		if p.MetricName == "stage_rank" {
			distinct[p.MetricValue] = true
		}
	}
	if len(distinct) != 3 {
		t.Fatalf("stage_rank read back as %d distinct value(s) of the 3 that were recorded: %v", len(distinct), distinct)
	}
}

// The fraction written alongside a value is matched back to it by the instant the pair was
// written at. Rounding that instant to the second let two nodes reporting inside one second
// overwrite each other's fraction, so a value ended up carrying a sibling's progress.
func TestGetTimeseriesKeepsEachSamplesOwnFractionWhenNodesReportInsideOneSecond(t *testing.T) {
	at := time.Now().UTC().Add(-time.Minute)
	server := recordedSamplesServer(at)
	defer server.Close()

	svc := &Service{
		store:        oneExperimentStore{exp: &domain.Experiment{ID: "exp-1", AgentID: "agent-1", CreatedAt: at.Add(-time.Hour)}},
		metricsDBURL: server.URL,
	}
	points, err := svc.GetTimeseries(context.Background(), "exp-1")
	if err != nil {
		t.Fatalf("GetTimeseries: %v", err)
	}
	for _, p := range points {
		if p.FractionComplete != 1 {
			t.Fatalf("sample %v carries fraction %v, not the 1 it was written with", p.MetricValue, p.FractionComplete)
		}
	}
}
