package httpapi

import (
	"context"
	"errors"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/registry"
)

// jobRecordsAPI serves what hangs off a job: the data it wrote, its metrics and its log tail.
type jobRecordsAPI struct {
	registry *registry.Service
	access   *auth.Access
	logger   *zap.Logger
}

func (r jobRecordsAPI) listData(ctx context.Context, in *struct {
	ID string `path:"id"`
}) (*struct{ Body []registry.DataObject }, error) {
	if err := r.access.Experiment(ctx, in.ID); err != nil {
		return nil, err
	}
	objects, err := r.registry.ListData(ctx, in.ID)
	if err != nil {
		if errors.Is(err, registry.ErrExperimentNotFound) {
			return nil, huma.Error404NotFound(err.Error())
		}
		return nil, auth.Internal(r.logger, "list experiment data", err, zap.String("id", in.ID))
	}
	return &struct{ Body []registry.DataObject }{Body: objects}, nil
}

func (r jobRecordsAPI) metrics(ctx context.Context, in *struct {
	ID string `path:"id"`
}) (*struct{ Body []*domain.MetricDataPoint }, error) {
	if err := r.access.Experiment(ctx, in.ID); err != nil {
		return nil, err
	}
	ts, err := r.registry.GetTimeseries(ctx, in.ID)
	if err != nil {
		if errors.Is(err, registry.ErrExperimentNotFound) {
			return nil, huma.Error404NotFound(err.Error())
		}
		return nil, auth.Internal(r.logger, "get metrics", err, zap.String("id", in.ID))
	}
	return &struct{ Body []*domain.MetricDataPoint }{Body: ts}, nil
}

func (r jobRecordsAPI) appendMetric(ctx context.Context, in *struct {
	ID   string `path:"id"`
	Body struct {
		MetricName       string  `json:"metric_name"`
		MetricBasis      string  `json:"metric_basis,omitempty"`
		FractionComplete float64 `json:"fraction_complete"`
		MetricValue      float64 `json:"metric_value"`
	}
}) (*struct{}, error) {
	if err := r.access.OwnExperiment(ctx, in.ID); err != nil {
		return nil, err
	}
	if err := r.registry.RecordMetric(ctx, in.ID, in.Body.MetricName, in.Body.MetricBasis, in.Body.FractionComplete, in.Body.MetricValue); err != nil {
		if errors.Is(err, registry.ErrExperimentNotFound) {
			return nil, huma.Error404NotFound(err.Error())
		}
		if errors.Is(err, registry.ErrInvalidMetric) {
			return nil, huma.Error422UnprocessableEntity(err.Error())
		}
		return nil, auth.Internal(r.logger, "append metric", err, zap.String("id", in.ID))
	}
	return nil, nil
}

func (r jobRecordsAPI) logTail(ctx context.Context, in *struct {
	ID string `path:"id"`
	N  int    `query:"n" default:"10" doc:"How many of the last reported lines to return. Capped by however many the owning cluster-agent last reported."`
}) (*struct{ Body []string }, error) {
	if err := r.access.Experiment(ctx, in.ID); err != nil {
		return nil, err
	}
	lines, err := r.registry.GetLogTail(ctx, in.ID, in.N)
	if err != nil {
		if errors.Is(err, registry.ErrExperimentNotFound) {
			return nil, huma.Error404NotFound(err.Error())
		}
		return nil, auth.Internal(r.logger, "get log tail", err, zap.String("id", in.ID))
	}
	return &struct{ Body []string }{Body: lines}, nil
}

func (r jobRecordsAPI) timeseries(ctx context.Context, in *struct {
	ID            string  `path:"id"`
	MetricName    string  `query:"metric_name" doc:"Required. One of the metric keys this platform experiment declares (GET /platform-experiments/{id} -> metrics[].key)."`
	LookbackHours float64 `query:"lookback_hours" doc:"How far back to plot, default 24. Each series is downsampled to about 500 points regardless of the window, so a longer window costs resolution rather than response size."`
	End           string  `query:"end" doc:"RFC3339. Where the window ends, default now; a time still ahead means now. Pass a finished platform experiment's ends_at so the window stays on its run."`
}) (*struct {
	Body struct {
		Series []*domain.AgentMetricSeries `json:"series"`
	}
}, error) {
	if err := r.access.PlatformExperiment(ctx, in.ID); err != nil {
		return nil, err
	}
	if in.MetricName == "" {
		return nil, huma.Error400BadRequest("metric_name is required")
	}
	lookback := 24 * time.Hour
	if in.LookbackHours > 0 {
		lookback = time.Duration(in.LookbackHours * float64(time.Hour))
	}
	var end time.Time
	if in.End != "" {
		parsed, err := time.Parse(time.RFC3339, in.End)
		if err != nil {
			return nil, huma.Error400BadRequest("end must be RFC3339")
		}
		end = parsed
	}
	series, err := r.registry.GetPlatformExperimentTimeseries(ctx, in.ID, in.MetricName, lookback, end)
	if err != nil {
		return nil, auth.Internal(r.logger, "platform experiment timeseries", err, zap.String("id", in.ID))
	}
	resp := &struct {
		Body struct {
			Series []*domain.AgentMetricSeries `json:"series"`
		}
	}{}
	resp.Body.Series = series
	return resp, nil
}
