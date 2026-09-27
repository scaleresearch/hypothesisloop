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

// The shared row reader requires greptime_timestamp and greptime_value columns and errors
// without them. A query that names its columns anything else therefore fails at runtime, not at
// compile time — and this one feeds job-to-node attribution, so its failure aborts the
// disbalance pass entirely rather than surfacing as a wrong answer.
func TestLatestExperimentNodeAsksForColumnsTheReaderCanParse(t *testing.T) {
	var asked string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.Query().Get("sql")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output":[{"records":{"schema":{"column_schemas":[
			{"name":"node","data_type":"String"},
			{"name":"greptime_timestamp","data_type":"TimestampMillisecond"},
			{"name":"greptime_value","data_type":"Int64"}]},
			"rows":[["node-b",1700000000000,1]]}}]}`))
	}))
	defer server.Close()

	node, found, err := LatestExperimentNode(context.Background(), server.URL, "exp-1", time.Now().UTC().Add(-time.Hour), time.Now().UTC())
	if err != nil {
		t.Fatalf("LatestExperimentNode: %v", err)
	}
	if !found || node != "node-b" {
		t.Fatalf("node = %q found = %v, want node-b", node, found)
	}
	for _, column := range []string{"greptime_timestamp", "greptime_value"} {
		if !strings.Contains(asked, column) {
			t.Errorf("query does not select %s, which the row reader requires: %s", column, asked)
		}
	}
}

// Ordering must come from each node's newest real sample. Asking on a grid whose step equalled
// the lookback put every node's last point on the same timestamp, so the winner was whichever
// series came back first — after a reschedule, as likely the node the job left as the one it
// moved to.
func TestLatestExperimentNodeOrdersByNewestSample(t *testing.T) {
	var asked string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.Query().Get("sql")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output":[{"records":{"schema":{"column_schemas":[
			{"name":"node","data_type":"String"},
			{"name":"greptime_timestamp","data_type":"TimestampMillisecond"},
			{"name":"greptime_value","data_type":"Int64"}]},
			"rows":[["node-new",1700000000000,1]]}}]}`))
	}))
	defer server.Close()

	if _, _, err := LatestExperimentNode(context.Background(), server.URL, "exp-1", time.Now().UTC().Add(-time.Hour), time.Now().UTC()); err != nil {
		t.Fatalf("LatestExperimentNode: %v", err)
	}
	if !strings.Contains(asked, "MAX(greptime_timestamp)") || !strings.Contains(asked, "ORDER BY 2 DESC") {
		t.Errorf("query does not order nodes by their newest sample: %s", asked)
	}
}

// The machine behind a node is written as labels of the node's own sample, each only when the
// runtime said it, and read back from the newest sample. A table no runtime has yet written a
// machine to has none of those columns, and a sample from a runtime that names the node alone
// has them empty: both read as the node with no machine.
func TestWhereAJobRanIsTheNodeAndTheMachineItsRuntimeReported(t *testing.T) {
	machine := &domain.JobMachine{InstanceID: "54479019", HostID: "11230", AcceleratorModel: "RTX 4090", PricePerHourUSD: 0.4544}
	labels := nodeLabels(JobStatusSample{ExperimentID: "exp-1", AdmittedNode: "rtx4090-0", AdmittedMachine: machine})
	want := map[string]string{
		"experiment_id": "exp-1", "node": "rtx4090-0", machineInstanceLabel: "54479019", machineHostLabel: "11230",
		machineAcceleratorLabel: "RTX 4090", machinePriceLabel: "0.4544",
	}
	if !reflect.DeepEqual(labels, want) {
		t.Fatalf("labels = %v, want %v: no label for the CPU model the runtime did not say", labels, want)
	}

	answers := map[string]string{
		"with a machine": `{"output":[{"records":{"schema":{"column_schemas":[
			{"name":"experiment_id"},{"name":"node"},{"name":"machine_instance_id"},{"name":"machine_host_id"},
			{"name":"machine_accelerator_model"},{"name":"machine_cpu_model"},{"name":"machine_price_per_hour_usd"},
			{"name":"greptime_timestamp"},{"name":"greptime_value"}]},
			"rows":[["exp-1","rtx4090-0","54479019","11230","RTX 4090",null,"0.4544",1700000000000,1]]}}]}`,
		"node alone": `{"output":[{"records":{"schema":{"column_schemas":[
			{"name":"experiment_id"},{"name":"node"},{"name":"greptime_timestamp"},{"name":"greptime_value"}]},
			"rows":[["exp-1","node-b",1700000000000,1]]}}]}`,
		"never reported": `{"output":[{"records":{"schema":{"column_schemas":[
			{"name":"experiment_id"},{"name":"node"},{"name":"greptime_timestamp"},{"name":"greptime_value"}]},"rows":[]}}]}`,
	}
	wants := map[string]*domain.RanOn{
		"with a machine": {Node: "rtx4090-0", Machine: machine},
		"node alone":     {Node: "node-b"},
		"never reported": nil,
	}
	for name, answer := range answers {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(answer))
		}))
		ranOn, err := LatestRanOn(context.Background(), server.URL, "exp-1", time.Now().UTC().Add(-time.Hour), time.Now().UTC())
		server.Close()
		if err != nil || !reflect.DeepEqual(ranOn, wants[name]) {
			t.Errorf("%s: ran on = %+v, %v, want %+v", name, ranOn, err, wants[name])
		}
	}
}
