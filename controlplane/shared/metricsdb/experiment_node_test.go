package metricsdb

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

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
	job := []*domain.Experiment{{ID: "exp-1", CreatedAt: time.Now().UTC().Add(-time.Hour)}}
	for name, answer := range answers {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(answer))
		}))
		ranOn, err := LatestRanOn(context.Background(), server.URL, job, time.Now().UTC())
		server.Close()
		if err != nil || !reflect.DeepEqual(ranOn["exp-1"], wants[name]) {
			t.Errorf("%s: ran on = %+v, %v, want %+v", name, ranOn, err, wants[name])
		}
	}
}

// A page of jobs is told where each ran by one query over the page, bounded by its oldest job,
// and a job with no node sample is left out of the answer.
func TestWhereAPageOfJobsRanIsOneQuery(t *testing.T) {
	var asked []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Query().Get("sql"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output":[{"records":{"schema":{"column_schemas":[
			{"name":"experiment_id"},{"name":"node"},{"name":"machine_host_id"},{"name":"greptime_timestamp"},{"name":"greptime_value"}]},
			"rows":[["exp-rented","rtx4090-0","16358",1700000000000,1],["exp-own","box-b",null,1700000000000,1]]}}]}`))
	}))
	defer server.Close()

	now := time.Now().UTC()
	oldest := now.Add(-3 * time.Hour)
	page := []*domain.Experiment{
		{ID: "exp-rented", CreatedAt: now.Add(-time.Hour)}, {ID: "exp-own", CreatedAt: oldest}, {ID: "exp-queued", CreatedAt: now.Add(-time.Minute)},
	}
	ranOn, err := LatestRanOn(context.Background(), server.URL, page, now)
	want := map[string]*domain.RanOn{
		"exp-rented": {Node: "rtx4090-0", Machine: &domain.JobMachine{HostID: "16358"}},
		"exp-own":    {Node: "box-b"},
	}
	if err != nil || !reflect.DeepEqual(ranOn, want) {
		t.Fatalf("ran on = %+v, %v, want %+v", ranOn, err, want)
	}
	if len(asked) != 1 {
		t.Fatalf("asked the metrics store %d times for a page of %d, want once", len(asked), len(page))
	}
	for _, part := range []string{"'exp-rented', 'exp-own', 'exp-queued'", fmt.Sprintf(">= %d::TimestampMillisecond", ObservationWindowStart(oldest).UnixMilli())} {
		if !strings.Contains(asked[0], part) {
			t.Errorf("query lacks %q: %s", part, asked[0])
		}
	}

	if none, err := LatestRanOn(context.Background(), server.URL, nil, now); err != nil || len(none) != 0 || len(asked) != 1 {
		t.Errorf("no jobs = %+v, %v after %d queries, want nothing and no query", none, err, len(asked))
	}
}
