// Package metricsdb is the single client for GreptimeDB (a Prometheus-remote-write- and
// PromQL-compatible TSDB). It is the only place in the control plane that speaks the
// remote-write/PromQL wire protocol — every metric sample (job-reported metrics, agent quota
// usage) is written and read through here, so there is exactly one store for "numbers that
// change over time" and Postgres never duplicates it.
package metricsdb

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/gogo/protobuf/proto"
	"github.com/golang/snappy"
)

// rwLabel/rwSample/rwTimeSeries/rwWriteRequest mirror prometheus/prompb's wire types — just
// enough of the remote-write protocol to push individual samples to GreptimeDB without pulling
// in the full prometheus/prometheus dependency tree.
type rwLabel struct {
	Name  string `protobuf:"bytes,1,opt,name=name,proto3" json:"name,omitempty"`
	Value string `protobuf:"bytes,2,opt,name=value,proto3" json:"value,omitempty"`
}

func (m *rwLabel) Reset()         {}
func (m *rwLabel) String() string { return fmt.Sprintf("%s=%q", m.Name, m.Value) }
func (m *rwLabel) ProtoMessage()  {}

type rwSample struct {
	Value     float64 `protobuf:"fixed64,1,opt,name=value,proto3" json:"value,omitempty"`
	Timestamp int64   `protobuf:"varint,2,opt,name=timestamp,proto3" json:"timestamp,omitempty"` // ms
}

func (m *rwSample) Reset()         {}
func (m *rwSample) String() string { return fmt.Sprintf("%v@%d", m.Value, m.Timestamp) }
func (m *rwSample) ProtoMessage()  {}

type rwTimeSeries struct {
	Labels  []*rwLabel  `protobuf:"bytes,1,rep,name=labels,proto3" json:"labels,omitempty"`
	Samples []*rwSample `protobuf:"bytes,2,rep,name=samples,proto3" json:"samples,omitempty"`
}

func (m *rwTimeSeries) Reset()         {}
func (m *rwTimeSeries) String() string { return fmt.Sprintf("%v", m.Labels) }
func (m *rwTimeSeries) ProtoMessage()  {}

type rwWriteRequest struct {
	Timeseries []*rwTimeSeries `protobuf:"bytes,1,rep,name=timeseries,proto3" json:"timeseries,omitempty"`
}

func (m *rwWriteRequest) Reset()         {}
func (m *rwWriteRequest) String() string { return fmt.Sprintf("%d series", len(m.Timeseries)) }
func (m *rwWriteRequest) ProtoMessage()  {}

// WriteGauge pushes a single (metric_name, labels, value) sample to GreptimeDB via Prometheus
// remote write, timestamped now. For delayed observations use WriteGaugeAt with the observation's
// collection timestamp.
func WriteGauge(ctx context.Context, dbURL, metricName string, labels map[string]string, value float64) error {
	return WriteGaugeAt(ctx, dbURL, metricName, labels, value, time.Now().UTC())
}

// WriteGaugeAt pushes a single (metric_name, labels, value) sample to GreptimeDB via Prometheus
// remote write, timestamped at `at` rather than call time. This matters for anything that can be
// delayed or retried (a node-agent heartbeat buffered through a network blip, a batch flush after
// an outage): the stored sample must carry the moment the event actually happened, not the moment
// it happened to arrive, or every downstream query recomputing "how long was this alive" would
// silently absorb the delivery delay as if it were real elapsed time.
func WriteGaugeAt(ctx context.Context, dbURL, metricName string, labels map[string]string, value float64, at time.Time) error {
	return WriteGaugesAt(ctx, dbURL, []GaugeSample{{MetricName: metricName, Labels: labels, Value: value, At: at}})
}

// GaugeSample is one (metric_name, labels, value, timestamp) point for WriteGaugesAt.
type GaugeSample struct {
	MetricName string
	Labels     map[string]string
	Value      float64
	At         time.Time
}

// WriteGaugesAt pushes any number of samples to GreptimeDB in a single Prometheus remote-write
// request. Prometheus remote write is designed to carry a batch of timeseries per request; a
// caller with several related samples to write at once (e.g. the node-agent's per-node push,
// which reports every pod on the node in one payload) should batch them here rather than issuing
// one HTTP round trip per sample — same durability and per-sample timestamp semantics as
// WriteGaugeAt, just fewer requests for the same data.
func WriteGaugesAt(ctx context.Context, dbURL string, samples []GaugeSample) error {
	if len(samples) == 0 {
		return nil
	}
	req := &rwWriteRequest{Timeseries: make([]*rwTimeSeries, 0, len(samples))}
	for _, s := range samples {
		ls := []*rwLabel{{Name: "__name__", Value: s.MetricName}}
		for k, v := range s.Labels {
			ls = append(ls, &rwLabel{Name: k, Value: v})
		}
		req.Timeseries = append(req.Timeseries, &rwTimeSeries{
			Labels:  ls,
			Samples: []*rwSample{{Value: s.Value, Timestamp: s.At.UnixMilli()}},
		})
	}

	raw, err := proto.Marshal(req)
	if err != nil {
		return fmt.Errorf("metricsdb.WriteGaugesAt: marshal: %w", err)
	}
	compressed := snappy.Encode(nil, raw)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		dbURL+"/v1/prometheus/write", bytes.NewReader(compressed))
	if err != nil {
		return fmt.Errorf("metricsdb.WriteGaugesAt: build request: %w", err)
	}
	httpReq.Header.Set("Content-Encoding", "snappy")
	httpReq.Header.Set("Content-Type", "application/x-protobuf")
	httpReq.Header.Set("X-Prometheus-Remote-Write-Version", "0.1.0")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("metricsdb.WriteGaugesAt: post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("metricsdb.WriteGaugesAt: greptimedb returned %d", resp.StatusCode)
	}
	return nil
}

// VectorSample is one series in a PromQL instant-query result: its labels and current value.
type VectorSample struct {
	Labels map[string]string
	Value  float64
	At     time.Time
}

// QueryVector executes a PromQL instant query against GreptimeDB's Prometheus-compatible query
// API and returns every series in the result vector.
func QueryVector(ctx context.Context, dbURL, promQL string) ([]VectorSample, error) {
	u, err := url.Parse(dbURL + "/v1/prometheus/api/v1/query")
	if err != nil {
		return nil, fmt.Errorf("metricsdb.QueryVector: %w", err)
	}
	q := u.Query()
	q.Set("query", promQL)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("metricsdb.QueryVector: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("metricsdb.QueryVector: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("metricsdb.QueryVector: read body: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("metricsdb.QueryVector: greptimedb returned %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Status    string `json:"status"`
		ErrorType string `json:"errorType"`
		Error     string `json:"error"`
		Data      struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string  `json:"metric"`
				Value  [2]json.RawMessage `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("metricsdb.QueryVector: unmarshal: %w", err)
	}
	if result.Status != "success" {
		return nil, fmt.Errorf("metricsdb.QueryVector: query failed (%s): %s", result.ErrorType, result.Error)
	}
	if result.Data.ResultType != "vector" {
		return nil, fmt.Errorf("metricsdb.QueryVector: expected vector result, got %q", result.Data.ResultType)
	}

	out := make([]VectorSample, 0, len(result.Data.Result))
	for i, r := range result.Data.Result {
		var tsFloat float64
		if err := json.Unmarshal(r.Value[0], &tsFloat); err != nil {
			return nil, fmt.Errorf("metricsdb.QueryVector: result %d timestamp: %w", i, err)
		}
		if math.IsNaN(tsFloat) || math.IsInf(tsFloat, 0) {
			return nil, fmt.Errorf("metricsdb.QueryVector: result %d timestamp is not finite", i)
		}
		if len(r.Value[1]) == 0 {
			return nil, fmt.Errorf("metricsdb.QueryVector: result %d has no value", i)
		}
		var valStr string
		if err := json.Unmarshal(r.Value[1], &valStr); err != nil {
			return nil, fmt.Errorf("metricsdb.QueryVector: result %d value: %w", i, err)
		}
		v, err := strconv.ParseFloat(valStr, 64)
		if err != nil {
			return nil, fmt.Errorf("metricsdb.QueryVector: result %d value %q: %w", i, valStr, err)
		}
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, fmt.Errorf("metricsdb.QueryVector: result %d is not finite: %q", i, valStr)
		}
		out = append(out, VectorSample{Labels: r.Metric, Value: v, At: time.UnixMilli(int64(tsFloat * 1000)).UTC()})
	}
	return out, nil
}

// RangePoint is one (timestamp, value) point from a range query's evaluation grid.
type RangePoint struct {
	Time  time.Time
	Value float64
}

// RangeSeries is one series from a range query: its labels and every grid point returned.
type RangeSeries struct {
	Labels map[string]string
	Points []RangePoint
}

// QueryRange executes a PromQL range query against GreptimeDB's Prometheus-compatible query API,
// evaluating promQL at every `step`-spaced point from start to end and returning every series in
// the result matrix. Every reader that needs to know when something actually happened (not just
// its current value) goes through this — see metricsdb.ObservedElapsedHours.
func QueryRange(ctx context.Context, dbURL, promQL string, start, end time.Time, step time.Duration) ([]RangeSeries, error) {
	if step <= 0 {
		return nil, fmt.Errorf("metricsdb.QueryRange: step must be positive")
	}
	if end.Before(start) {
		return nil, fmt.Errorf("metricsdb.QueryRange: end precedes start")
	}
	u, err := url.Parse(dbURL + "/v1/prometheus/api/v1/query_range")
	if err != nil {
		return nil, fmt.Errorf("metricsdb.QueryRange: %w", err)
	}
	q := u.Query()
	q.Set("query", promQL)
	q.Set("start", strconv.FormatInt(start.Unix(), 10))
	q.Set("end", strconv.FormatInt(end.Unix(), 10))
	q.Set("step", strconv.FormatFloat(step.Seconds(), 'f', -1, 64))
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("metricsdb.QueryRange: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("metricsdb.QueryRange: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("metricsdb.QueryRange: read body: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("metricsdb.QueryRange: greptimedb returned %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Status    string `json:"status"`
		ErrorType string `json:"errorType"`
		Error     string `json:"error"`
		Data      struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string    `json:"metric"`
				Values [][2]json.RawMessage `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("metricsdb.QueryRange: unmarshal: %w", err)
	}
	if result.Status != "success" {
		return nil, fmt.Errorf("metricsdb.QueryRange: query failed (%s): %s", result.ErrorType, result.Error)
	}
	if result.Data.ResultType != "matrix" {
		return nil, fmt.Errorf("metricsdb.QueryRange: expected matrix result, got %q", result.Data.ResultType)
	}

	out := make([]RangeSeries, 0, len(result.Data.Result))
	for seriesIndex, r := range result.Data.Result {
		series := RangeSeries{Labels: r.Metric, Points: make([]RangePoint, 0, len(r.Values))}
		for pointIndex, v := range r.Values {
			var tsFloat float64
			if err := json.Unmarshal(v[0], &tsFloat); err != nil {
				return nil, fmt.Errorf("metricsdb.QueryRange: series %d point %d timestamp: %w", seriesIndex, pointIndex, err)
			}
			if math.IsNaN(tsFloat) || math.IsInf(tsFloat, 0) {
				return nil, fmt.Errorf("metricsdb.QueryRange: series %d point %d timestamp is not finite", seriesIndex, pointIndex)
			}
			var valStr string
			if err := json.Unmarshal(v[1], &valStr); err != nil {
				return nil, fmt.Errorf("metricsdb.QueryRange: series %d point %d value: %w", seriesIndex, pointIndex, err)
			}
			val, err := strconv.ParseFloat(valStr, 64)
			if err != nil {
				return nil, fmt.Errorf("metricsdb.QueryRange: series %d point %d value %q: %w", seriesIndex, pointIndex, valStr, err)
			}
			if math.IsNaN(val) || math.IsInf(val, 0) {
				return nil, fmt.Errorf("metricsdb.QueryRange: series %d point %d is not finite: %q", seriesIndex, pointIndex, valStr)
			}
			series.Points = append(series.Points, RangePoint{Time: time.UnixMilli(int64(tsFloat * 1000)).UTC(), Value: val})
		}
		out = append(out, series)
	}
	return out, nil
}

// QueryAgentValues runs an instant PromQL query and returns a map of agent_id → value, for
// queries whose result vector is grouped by the agent_id label (e.g. "max by (agent_id) (...)").
func QueryAgentValues(ctx context.Context, dbURL, promQL string) (map[string]float64, error) {
	samples, err := QueryVector(ctx, dbURL, promQL)
	if err != nil {
		return nil, err
	}
	out := make(map[string]float64, len(samples))
	for _, s := range samples {
		agentID := s.Labels["agent_id"]
		if agentID == "" {
			return nil, fmt.Errorf("metricsdb.QueryAgentValues: result missing agent_id")
		}
		if _, exists := out[agentID]; exists {
			return nil, fmt.Errorf("metricsdb.QueryAgentValues: duplicate agent_id %q", agentID)
		}
		out[agentID] = s.Value
	}
	return out, nil
}
