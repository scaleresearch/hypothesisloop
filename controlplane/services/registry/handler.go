package registry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// Handler wires the registry service to HTTP routes, registered via RegisterHuma.
type Handler struct {
	svc    *Service
	logger *zap.Logger
}

// NewHandler returns a Handler for the given service.
func NewHandler(svc *Service, logger *zap.Logger) *Handler {
	return &Handler{svc: svc, logger: logger}
}

// hypothesisResponse wraps a Hypothesis with whether registration found a pre-existing
// equivalent row.
type hypothesisResponse struct {
	*domain.Hypothesis
	AlreadyExisted bool `json:"already_existed"`
}

// hypothesisWithJobs bundles a hypothesis with the jobs submitted against it and the
// findings filed against it.
type hypothesisWithJobs struct {
	*domain.Hypothesis
	Jobs     []*domain.Experiment        `json:"jobs"`
	Findings []*domain.HypothesisFinding `json:"findings"`
	Comments []*domain.HypothesisComment `json:"comments"`
}

// RegisterHuma registers every registry operation at its full path (/registry/...).
func RegisterHuma(doc *apidocs.Doc, h *Handler) {
	// ---- experiments (registry records) ----

	// Reading experiments — the list and a single row, both with phase_detail — lives on the
	// scheduler service only (GET /experiments, GET /experiments/{id}); no endpoint duplicating
	// it here. What is unique to the registry is what hangs off an experiment: its lineage,
	// metrics, logs and hypotheses.

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "registry-get-lineage", Method: "GET", Path: "/registry/experiments/{id}/lineage",
		Summary: "Get an experiment's lineage chain", Tags: []string{"experiments"},
		Description: "What a result was derived from.",
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*struct{ Body []*domain.Experiment }, error) {
		chain, err := h.svc.GetLineage(ctx, in.ID)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		return &struct{ Body []*domain.Experiment }{Body: chain}, nil
	})

	// ---- metrics ----

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "registry-get-metrics", Method: "GET", Path: "/registry/experiments/{id}/metrics",
		Summary: "Get an experiment's metric timeseries", Tags: []string{"metrics"},
		Description: "{metric_name, fraction_complete, metric_value} points.",
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*struct{ Body []*domain.MetricDataPoint }, error) {
		ts, err := h.svc.GetTimeseries(ctx, in.ID)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		return &struct{ Body []*domain.MetricDataPoint }{Body: ts}, nil
	})

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "registry-append-metric", Method: "POST", Path: "/registry/experiments/{id}/metrics",
		Summary: "Append a metric sample for an experiment", Tags: []string{"metrics"},
		DefaultStatus: 204,
		Description:   "Records one {metric_name, fraction_complete, metric_value} sample. No server-side validation of the value — never fabricate one.",
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Body struct {
			MetricName       string  `json:"metric_name"`
			FractionComplete float64 `json:"fraction_complete"`
			MetricValue      float64 `json:"metric_value"`
		}
	}) (*struct{}, error) {
		name := in.Body.MetricName
		if name == "" {
			name = "default"
		}
		if err := h.svc.RecordMetric(ctx, in.ID, name, in.Body.FractionComplete, in.Body.MetricValue); err != nil {
			if errors.Is(err, ErrInvalidMetric) {
				return nil, huma.Error422UnprocessableEntity(err.Error())
			}
			h.logger.Error("record metric", zap.String("id", in.ID), zap.Error(err))
			return nil, huma.Error500InternalServerError(err.Error())
		}
		return nil, nil
	})

	// No POST here: log tails are never pushed by the job/research-agent side of the API --
	// only a cluster-agent may report a job's log tail (via /internal/clusters/{name}/status,
	// see clusteragentapi), same as job phase. This is read-only.
	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "registry-get-log-tail", Method: "GET", Path: "/registry/experiments/{id}/logs",
		Summary: "Get the job's most recently reported log tail", Tags: []string{"metrics"},
		Description: "Returns up to the last `n` lines the owning cluster-agent last reported " +
			"(default 10 -- pass a larger ?n for more, up to however many were last reported). " +
			"Empty (not an error) if none has been reported yet.",
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
		N  int    `query:"n" default:"10"`
	}) (*struct{ Body []string }, error) {
		lines, err := h.svc.GetLogTail(ctx, in.ID, in.N)
		if err != nil {
			if errors.Is(err, ErrInvalidMetric) {
				return nil, huma.Error404NotFound(err.Error())
			}
			h.logger.Error("get log tail", zap.String("id", in.ID), zap.Error(err))
			return nil, huma.Error500InternalServerError(err.Error())
		}
		return &struct{ Body []string }{Body: lines}, nil
	})

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "registry-platform-experiment-timeseries", Method: "GET",
		Path:    "/registry/platform-experiments/{id}/metrics-timeseries",
		Summary: "Get per-job metric history across a platform experiment", Tags: []string{"metrics"},
		Description: "One series per job so a dashboard can plot competing agents. Requires ?metric_name; optional ?lookback_hours (default 24).",
	}, func(ctx context.Context, in *struct {
		ID            string  `path:"id"`
		MetricName    string  `query:"metric_name"`
		LookbackHours float64 `query:"lookback_hours"`
	}) (*struct {
		Body struct {
			Series []*domain.AgentMetricSeries `json:"series"`
		}
	}, error) {
		if in.MetricName == "" {
			return nil, huma.Error400BadRequest("metric_name is required")
		}
		lookback := 24 * time.Hour
		if in.LookbackHours > 0 {
			lookback = time.Duration(in.LookbackHours * float64(time.Hour))
		}
		series, err := h.svc.GetPlatformExperimentTimeseries(ctx, in.ID, in.MetricName, lookback)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		resp := &struct {
			Body struct {
				Series []*domain.AgentMetricSeries `json:"series"`
			}
		}{}
		resp.Body.Series = series
		return resp, nil
	})

	// ---- hypotheses ----

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "registry-register-hypothesis", Method: "POST", Path: "/registry/hypotheses",
		Summary: "Register a hypothesis", Tags: []string{"hypotheses"},
		DefaultStatus: 201,
		Description: "Idempotent within a platform_experiment_id: text equivalent to an existing hypothesis " +
			"(modulo case/whitespace) returns that row with 200 and already_existed=true, a new one 201. The " +
			"returned id is metadata.hypothesis_id for the job testing it; every job must reference a " +
			"hypothesis from its own platform experiment.",
	}, func(ctx context.Context, in *struct {
		Body struct {
			AgentID              string `json:"agent_id"`
			PlatformExperimentID string `json:"platform_experiment_id"`
			Text                 string `json:"text"`
		}
	}) (*struct {
		Status int
		Body   hypothesisResponse
	}, error) {
		b := in.Body
		if b.AgentID == "" {
			return nil, huma.Error400BadRequest("agent_id is required")
		}
		if b.PlatformExperimentID == "" {
			return nil, huma.Error400BadRequest("platform_experiment_id is required")
		}
		if b.Text == "" {
			return nil, huma.Error400BadRequest("text is required")
		}
		hyp, existed, err := h.svc.RegisterHypothesis(ctx, b.AgentID, b.PlatformExperimentID, b.Text)
		if err != nil {
			h.logger.Error("register hypothesis", zap.Error(err))
			return nil, huma.Error500InternalServerError(err.Error())
		}
		// Preserve the pre-Huma status semantics: 200 when the equivalent hypothesis already
		// existed (idempotent hit), 201 when a new row was created.
		status := http.StatusCreated
		if existed {
			status = http.StatusOK
		}
		return &struct {
			Status int
			Body   hypothesisResponse
		}{Status: status, Body: hypothesisResponse{Hypothesis: hyp, AlreadyExisted: existed}}, nil
	})

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "registry-list-hypotheses", Method: "GET", Path: "/registry/hypotheses",
		Summary: "List hypotheses for a platform experiment", Tags: []string{"hypotheses"},
		Description: "Requires ?platform_experiment_id; optional ?agent for one agent's own, " +
			"?status (open|confirmed|refuted|inconclusive) for just that status, ?limit " +
			"(default/max 200), most recent first. Rows carry finding_count/comment_count — drill " +
			"into GET /registry/hypotheses/{id} only where a count or relevance earns it.",
	}, func(ctx context.Context, in *struct {
		PlatformExperimentID string                  `query:"platform_experiment_id"`
		Agent                string                  `query:"agent"`
		Status               domain.HypothesisStatus `query:"status"`
		Limit                int                     `query:"limit"`
	}) (*struct{ Body []*db.HypothesisListItem }, error) {
		if in.PlatformExperimentID == "" {
			return nil, huma.Error400BadRequest("platform_experiment_id is required")
		}
		if in.Status != "" && !domain.ValidHypothesisStatus(in.Status) {
			return nil, huma.Error400BadRequest(fmt.Sprintf("invalid status %q", in.Status))
		}
		hs, err := h.svc.ListHypotheses(ctx, in.PlatformExperimentID, in.Agent, in.Status, in.Limit)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		return &struct{ Body []*db.HypothesisListItem }{Body: hs}, nil
	})

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "registry-get-hypothesis", Method: "GET", Path: "/registry/hypotheses/{id}",
		Summary: "Get a hypothesis with its jobs, findings, and comments", Tags: []string{"hypotheses"},
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*struct{ Body hypothesisWithJobs }, error) {
		hyp, err := h.svc.GetHypothesis(ctx, in.ID)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		if hyp == nil {
			return nil, huma.Error404NotFound("hypothesis not found")
		}
		jobs, err := h.svc.List(ctx, domain.ExperimentFilter{HypothesisID: in.ID})
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		findings, err := h.svc.ListHypothesisFindings(ctx, in.ID)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		comments, err := h.svc.ListHypothesisComments(ctx, in.ID)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		return &struct{ Body hypothesisWithJobs }{Body: hypothesisWithJobs{Hypothesis: hyp, Jobs: jobs, Findings: findings, Comments: comments}}, nil
	})

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "registry-add-hypothesis-comment", Method: "POST", Path: "/registry/hypotheses/{id}/comments",
		Summary: "Add a comment to a hypothesis", Tags: []string{"hypotheses"},
		DefaultStatus: 201,
		Description: "A freeform note with no job behind it (abandon/revise/cross-reference), as opposed to " +
			"a finding, which requires a terminal job. Read the hypothesis's existing comments (GET " +
			"/registry/hypotheses/{id}) first — don't re-record a conclusion already there.",
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Body struct {
			AgentID string `json:"agent_id"`
			Text    string `json:"text"`
		}
	}) (*struct {
		Status int
		Body   *domain.HypothesisComment
	}, error) {
		if in.Body.AgentID == "" {
			return nil, huma.Error400BadRequest("agent_id is required")
		}
		if in.Body.Text == "" {
			return nil, huma.Error400BadRequest("text is required")
		}
		c, err := h.svc.AddHypothesisComment(ctx, in.ID, in.Body.AgentID, in.Body.Text)
		if err != nil {
			if errors.Is(err, db.ErrUnknownAgent) {
				return nil, huma.Error400BadRequest("unknown_agent: " + in.Body.AgentID)
			}
			h.logger.Error("add hypothesis comment", zap.Error(err))
			return nil, huma.Error500InternalServerError(err.Error())
		}
		return &struct {
			Status int
			Body   *domain.HypothesisComment
		}{Status: http.StatusCreated, Body: c}, nil
	})

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "registry-set-hypothesis-status", Method: "POST", Path: "/registry/hypotheses/{id}/status",
		Summary: "Set a hypothesis's status", Tags: []string{"hypotheses"},
		Description: "open (default) / confirmed (validated as a real improvement) / refuted " +
			"(confidently established as not working) / inconclusive (noisy or not worth drilling " +
			"into). Only the registering agent_id may set it; anyone else gets 403.",
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Body struct {
			AgentID string                  `json:"agent_id"`
			Status  domain.HypothesisStatus `json:"status"`
		}
	}) (*struct{ Body *domain.Hypothesis }, error) {
		if in.Body.AgentID == "" {
			return nil, huma.Error400BadRequest("agent_id is required")
		}
		if !domain.ValidHypothesisStatus(in.Body.Status) {
			return nil, huma.Error400BadRequest("status must be one of: open, confirmed, refuted, inconclusive")
		}
		hyp, err := h.svc.SetHypothesisStatus(ctx, in.ID, in.Body.AgentID, in.Body.Status)
		if err != nil {
			switch {
			case errors.Is(err, ErrHypothesisNotFound):
				return nil, huma.Error404NotFound("hypothesis not found")
			case errors.Is(err, ErrHypothesisNotOwner):
				return nil, huma.Error403Forbidden("agent_id does not own this hypothesis")
			default:
				h.logger.Error("set hypothesis status", zap.Error(err))
				return nil, huma.Error500InternalServerError(err.Error())
			}
		}
		return &struct{ Body *domain.Hypothesis }{Body: hyp}, nil
	})
}
