package registry

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
	"github.com/scaleresearch/openresearch/controlplane/shared/metricsdb"
)

// Store is the persistence interface required by the registry service.
type Store interface {
	CreateExperiment(ctx context.Context, exp *domain.Experiment) error
	GetExperiment(ctx context.Context, id string) (*domain.Experiment, error)
	ListExperiments(ctx context.Context, filter domain.ExperimentFilter) ([]*domain.Experiment, error)
	UpdateExperiment(ctx context.Context, exp *domain.Experiment) error
	MarkStarted(ctx context.Context, id string) (bool, error)
	GetLineage(ctx context.Context, experimentID string) ([]*domain.Experiment, error)
	// FindOrCreateHypothesis registers a hypothesis, or returns the existing row (and true)
	// if one with equivalent normalized text already exists — the real uniqueness check.
	FindOrCreateHypothesis(ctx context.Context, agentID, text string) (h *domain.Hypothesis, alreadyExisted bool, err error)
	GetHypothesis(ctx context.Context, id string) (*domain.Hypothesis, error)
	ListHypotheses(ctx context.Context) ([]*domain.Hypothesis, error)
}

// Service manages experiments and their metric timeseries.
type Service struct {
	store        Store
	logger       *zap.Logger
	metricsDBURL string
}

// New returns a new registry Service.
func New(store Store, logger *zap.Logger, metricsDBURL string) *Service {
	return &Service{
		store:        store,
		logger:       logger,
		metricsDBURL: metricsDBURL,
	}
}

// newUUIDv7 generates a UUIDv7-formatted string: 48-bit unix-ms timestamp,
// version nibble 7, 12 random bits, variant bits, then 62 random bits.
func newUUIDv7() (string, error) {
	var rnd [10]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", fmt.Errorf("rand: %w", err)
	}

	ms := uint64(time.Now().UnixMilli())

	// Build 16 raw bytes.
	var b [16]byte
	// bytes 0-5: 48-bit timestamp
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	// bytes 6-7: version (7) | 12 random bits
	rand12 := binary.BigEndian.Uint16(rnd[0:2]) & 0x0fff
	binary.BigEndian.PutUint16(b[6:8], 0x7000|rand12)
	// bytes 8-15: variant (10xx) | 62 random bits
	copy(b[8:], rnd[2:])
	b[8] = (b[8] & 0x3f) | 0x80

	return fmt.Sprintf(
		"%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16],
	), nil
}

// Register assigns a new UUIDv7 ID and persists the experiment.
func (s *Service) Register(ctx context.Context, exp *domain.Experiment) error {
	id, err := newUUIDv7()
	if err != nil {
		return fmt.Errorf("registry.Register: generate id: %w", err)
	}
	exp.ID = id
	now := time.Now().UTC()
	exp.CreatedAt = now
	exp.UpdatedAt = now
	if exp.Status == "" {
		exp.Status = domain.StatusQueued
	}
	if err := s.store.CreateExperiment(ctx, exp); err != nil {
		return fmt.Errorf("registry.Register: store: %w", err)
	}
	s.logger.Info("experiment registered", zap.String("id", exp.ID), zap.String("agent", exp.AgentID))
	return nil
}

// Get returns a single experiment by ID.
func (s *Service) Get(ctx context.Context, id string) (*domain.Experiment, error) {
	exp, err := s.store.GetExperiment(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("registry.Get: %w", err)
	}
	return exp, nil
}

// List returns experiments matching the filter.
func (s *Service) List(ctx context.Context, filter domain.ExperimentFilter) ([]*domain.Experiment, error) {
	exps, err := s.store.ListExperiments(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("registry.List: %w", err)
	}
	return exps, nil
}

// UpdateStatus updates the status of an experiment.
// When transitioning to RUNNING it uses MarkStarted so that started_at is set exactly once.
func (s *Service) UpdateStatus(ctx context.Context, id string, status domain.ExperimentStatus) error {
	if status == domain.StatusRunning {
		if _, err := s.store.MarkStarted(ctx, id); err != nil {
			return fmt.Errorf("registry.UpdateStatus: mark started: %w", err)
		}
		s.logger.Info("experiment status updated", zap.String("id", id), zap.String("status", string(status)))
		return nil
	}

	exp, err := s.store.GetExperiment(ctx, id)
	if err != nil {
		return fmt.Errorf("registry.UpdateStatus: get: %w", err)
	}
	exp.Status = status
	exp.UpdatedAt = time.Now().UTC()
	if err := s.store.UpdateExperiment(ctx, exp); err != nil {
		return fmt.Errorf("registry.UpdateStatus: store: %w", err)
	}
	s.logger.Info("experiment status updated", zap.String("id", id), zap.String("status", string(status)))
	return nil
}

// RegisterHypothesis registers a new hypothesis, or returns the existing one (with
// alreadyExisted=true) if an equivalent one (by normalized text) was already registered.
// Agents should call this — and retrieve the returned ID — before submitting an experiment,
// since ExperimentMeta.HypothesisID is required.
func (s *Service) RegisterHypothesis(ctx context.Context, agentID, text string) (h *domain.Hypothesis, alreadyExisted bool, err error) {
	h, alreadyExisted, err = s.store.FindOrCreateHypothesis(ctx, agentID, text)
	if err != nil {
		return nil, false, fmt.Errorf("registry.RegisterHypothesis: %w", err)
	}
	s.logger.Info("hypothesis registered",
		zap.String("id", h.ID), zap.String("agent", agentID), zap.Bool("already_existed", alreadyExisted))
	return h, alreadyExisted, nil
}

// GetHypothesis returns a single hypothesis by ID.
func (s *Service) GetHypothesis(ctx context.Context, id string) (*domain.Hypothesis, error) {
	h, err := s.store.GetHypothesis(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("registry.GetHypothesis: %w", err)
	}
	return h, nil
}

// ListHypotheses returns every registered hypothesis, most recent first.
func (s *Service) ListHypotheses(ctx context.Context) ([]*domain.Hypothesis, error) {
	hs, err := s.store.ListHypotheses(ctx)
	if err != nil {
		return nil, fmt.Errorf("registry.ListHypotheses: %w", err)
	}
	return hs, nil
}

// GetLineage returns the ancestor chain of an experiment (oldest first).
func (s *Service) GetLineage(ctx context.Context, id string) ([]*domain.Experiment, error) {
	lineage, err := s.store.GetLineage(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("registry.GetLineage: %w", err)
	}
	return lineage, nil
}

// RecordMetric pushes a metric datapoint directly to GreptimeDB via Prometheus remote write.
// Labels: job_id, platform_experiment_id, agent_id, metric_name.
// These labels enable phase-2 percentile queries to filter and group by experiment/agent.
func (s *Service) RecordMetric(ctx context.Context, experimentID, metricName string, fractionComplete, value float64) error {
	// Look up experiment to get agent_id and platform_experiment_id for labels.
	exp, err := s.store.GetExperiment(ctx, experimentID)
	if err != nil {
		return fmt.Errorf("registry.RecordMetric: get experiment: %w", err)
	}
	agentID := ""
	platformExpID := ""
	if exp != nil {
		agentID = exp.AgentID
		platformExpID = exp.PlatformExperimentID
	}

	labels := map[string]string{
		"job_id":                 experimentID,
		"platform_experiment_id": platformExpID,
		"agent_id":               agentID,
		"metric_name":            metricName,
	}

	if err := metricsdb.WriteGauge(ctx, s.metricsDBURL, "experiment_metric_value", labels, value); err != nil {
		return fmt.Errorf("registry.RecordMetric: %w", err)
	}
	return nil
}

// prometheusRangeResult is the response shape for a PromQL range query (matrix), which
// returns every sample in the window — an instant query only ever returns the single latest
// value per series, which cannot show trend-over-time.
type prometheusRangeResult struct {
	Data struct {
		Result []struct {
			Metric map[string]string `json:"metric"`
			Values [][2]interface{}  `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

// GetPlatformExperimentTimeseries returns the full metric history (not just the latest
// value) for every job in platformExpID, one AgentMetricSeries per job — the data a
// competing-agents-over-time dashboard plots. Uses a PromQL range query, since an instant
// query (GetTimeseries) only ever returns the latest sample per series.
func (s *Service) GetPlatformExperimentTimeseries(ctx context.Context, platformExpID, metricName string, lookback time.Duration) ([]*domain.AgentMetricSeries, error) {
	if lookback <= 0 {
		lookback = 24 * time.Hour
	}
	end := time.Now().UTC()
	start := end.Add(-lookback)
	step := lookback / 500 // cap at ~500 points/series regardless of window size
	if step < time.Second {
		step = time.Second
	}

	query := fmt.Sprintf(`experiment_metric_value{platform_experiment_id=%q, metric_name=%q}`, platformExpID, metricName)
	apiURL := fmt.Sprintf("%s/v1/prometheus/api/v1/query_range?query=%s&start=%d&end=%d&step=%ds",
		s.metricsDBURL, url.QueryEscape(query), start.Unix(), end.Unix(), int(step.Seconds()))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("registry.GetPlatformExperimentTimeseries: build request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("registry.GetPlatformExperimentTimeseries: query prometheus: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("registry.GetPlatformExperimentTimeseries: read body: %w", err)
	}
	var result prometheusRangeResult
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("registry.GetPlatformExperimentTimeseries: decode response: %w", err)
	}

	out := make([]*domain.AgentMetricSeries, 0, len(result.Data.Result))
	for _, r := range result.Data.Result {
		series := &domain.AgentMetricSeries{
			AgentID:      r.Metric["agent_id"],
			ExperimentID: r.Metric["job_id"],
			MetricName:   r.Metric["metric_name"],
		}
		for _, v := range r.Values {
			var ts float64
			switch t := v[0].(type) {
			case float64:
				ts = t
			}
			var val float64
			if s, ok := v[1].(string); ok {
				fmt.Sscanf(s, "%f", &val)
			}
			series.Points = append(series.Points, domain.MetricSeriesPoint{
				Timestamp: time.Unix(int64(ts), 0).UTC(),
				Value:     val,
			})
		}
		out = append(out, series)
	}
	return out, nil
}

// GetTimeseries queries GreptimeDB (via its Prometheus-compatible PromQL API) for the
// latest metric values for an experiment.
func (s *Service) GetTimeseries(ctx context.Context, experimentID string) ([]*domain.MetricDataPoint, error) {
	// Range query, not instant: a job's metric history spans however long it's been running,
	// and a single job rarely runs longer than a day — 24h back is a safe, generous window.
	end := time.Now().UTC()
	start := end.Add(-24 * time.Hour)

	query := fmt.Sprintf(`experiment_metric_value{job_id=%q}`, experimentID)
	apiURL := fmt.Sprintf("%s/v1/prometheus/api/v1/query_range?query=%s&start=%d&end=%d&step=5s",
		s.metricsDBURL, url.QueryEscape(query), start.Unix(), end.Unix())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("registry.GetTimeseries: build request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("registry.GetTimeseries: query prometheus: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("registry.GetTimeseries: read body: %w", err)
	}

	var result prometheusRangeResult
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("registry.GetTimeseries: decode response: %w", err)
	}

	var out []*domain.MetricDataPoint
	for _, r := range result.Data.Result {
		for _, v := range r.Values {
			var ts float64
			if t, ok := v[0].(float64); ok {
				ts = t
			}
			var val float64
			if s, ok := v[1].(string); ok {
				fmt.Sscanf(s, "%f", &val)
			}
			out = append(out, &domain.MetricDataPoint{
				ExperimentID: experimentID,
				MetricName:   r.Metric["metric_name"],
				MetricValue:  val,
				RecordedAt:   time.Unix(int64(ts), 0).UTC(),
			})
		}
	}
	return out, nil
}
