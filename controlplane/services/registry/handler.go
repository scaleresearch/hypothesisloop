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
// findings filed against it. Each of the three lists is one bounded page of a set that only
// ever grows, so each carries the full count it was drawn from — a caller can always tell a
// short list from a truncated one, and page the rest with ?limit/?offset.
type hypothesisWithJobs struct {
	*domain.Hypothesis
	Jobs         []*domain.Experiment        `json:"jobs"`
	JobCount     int                         `json:"job_count"`
	Findings     []*domain.HypothesisFinding `json:"findings"`
	FindingCount int                         `json:"finding_count"`
	Comments     []*domain.HypothesisComment `json:"comments"`
	CommentCount int                         `json:"comment_count"`
}

// RegisterHuma registers the registry's operations: what hangs off an experiment (lineage,
// metrics, logs) and the hypothesis notebook. Paths sit under the resource they belong to —
// /experiments/{id}/... and /hypotheses/... — so a caller never has to know which service
// implements them; they are all registered on the one API doc alongside scheduler and quota.
// maxHypothesisDetailLimit bounds each of the three sub-lists GET /hypotheses/{id} returns,
// matching the ceiling every other list read here uses.
const maxHypothesisDetailLimit = 200

func RegisterHuma(doc *apidocs.Doc, h *Handler) {
	// ---- what hangs off an experiment ----

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "get-lineage", Method: "GET", Path: "/experiments/{id}/lineage",
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
		OperationID: "get-metrics", Method: "GET", Path: "/experiments/{id}/metrics",
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
		OperationID: "append-metric", Method: "POST", Path: "/experiments/{id}/metrics",
		Summary: "Append a metric sample for an experiment", Tags: []string{"metrics"},
		DefaultStatus: 204,
		Description: "Records one {metric_name, fraction_complete, metric_value} sample. No server-side validation of the value — never fabricate one. " +
			"metric_basis defaults to \"raw\": set it only when this value is not on the metric's usual scale (e.g. a different denormalization " +
			"reference than normal) so it is never silently ranked against unmodified runs.",
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Body struct {
			MetricName       string  `json:"metric_name"`
			MetricBasis      string  `json:"metric_basis,omitempty"`
			FractionComplete float64 `json:"fraction_complete"`
			MetricValue      float64 `json:"metric_value"`
		}
	}) (*struct{}, error) {
		name := in.Body.MetricName
		if name == "" {
			name = "default"
		}
		if err := h.svc.RecordMetric(ctx, in.ID, name, in.Body.MetricBasis, in.Body.FractionComplete, in.Body.MetricValue); err != nil {
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
		OperationID: "get-log-tail", Method: "GET", Path: "/experiments/{id}/logs",
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
		OperationID: "platform-experiment-timeseries", Method: "GET",
		Path:    "/platform-experiments/{id}/metrics-timeseries",
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
		OperationID: "register-hypothesis", Method: "POST", Path: "/hypotheses",
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
		OperationID: "list-hypotheses", Method: "GET", Path: "/hypotheses",
		Summary: "List hypotheses, optionally scoped to one platform experiment", Tags: []string{"hypotheses"},
		Description: "Optional ?platform_experiment_id restricts to one pool; omitted, every " +
			"platform experiment's hypotheses are candidates (the operator-facing global view). " +
			"Optional ?agent for one agent's own, ?status (open|confirmed|refuted|inconclusive) " +
			"for just that status, ?limit (default/max 200) and ?offset, most recent first. " +
			"Total match count is returned in the X-Total-Count response header. Rows carry " +
			"finding_count/comment_count — drill into GET /hypotheses/{id} only where a count or " +
			"relevance earns it.",
	}, func(ctx context.Context, in *struct {
		PlatformExperimentID string                  `query:"platform_experiment_id"`
		Agent                string                  `query:"agent"`
		Status               domain.HypothesisStatus `query:"status"`
		Limit                int                     `query:"limit"`
		Offset               int                     `query:"offset"`
	}) (*struct {
		Body       []*db.HypothesisListItem
		TotalCount int `header:"X-Total-Count"`
	}, error) {
		if in.Status != "" && !domain.ValidHypothesisStatus(in.Status) {
			return nil, huma.Error400BadRequest(fmt.Sprintf("invalid status %q", in.Status))
		}
		if in.Limit < 0 || in.Offset < 0 {
			return nil, huma.Error400BadRequest("limit and offset must not be negative")
		}
		hs, err := h.svc.ListHypotheses(ctx, in.PlatformExperimentID, in.Agent, in.Status, in.Limit, in.Offset)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		total, err := h.svc.CountHypotheses(ctx, in.PlatformExperimentID, in.Agent, in.Status)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		return &struct {
			Body       []*db.HypothesisListItem
			TotalCount int `header:"X-Total-Count"`
		}{Body: hs, TotalCount: total}, nil
	})

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "get-hypothesis", Method: "GET", Path: "/hypotheses/{id}",
		Summary: "Get a hypothesis with its jobs, findings, and comments", Tags: []string{"hypotheses"},
		Description: "?limit (default/max 200) and ?offset apply to each of jobs, findings and " +
			"comments alike, oldest first; job_count/finding_count/comment_count report the full " +
			"size of each set so a caller can page the rest.",
	}, func(ctx context.Context, in *struct {
		ID     string `path:"id"`
		Limit  int    `query:"limit"`
		Offset int    `query:"offset"`
	}) (*struct{ Body hypothesisWithJobs }, error) {
		if in.Limit < 0 || in.Offset < 0 {
			return nil, huma.Error400BadRequest("limit and offset must not be negative")
		}
		if in.Limit <= 0 || in.Limit > maxHypothesisDetailLimit {
			in.Limit = maxHypothesisDetailLimit
		}
		hyp, err := h.svc.GetHypothesis(ctx, in.ID)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		if hyp == nil {
			return nil, huma.Error404NotFound("hypothesis not found")
		}
		jobFilter := domain.ExperimentFilter{HypothesisID: in.ID}
		jobs, err := h.svc.List(ctx, domain.ExperimentFilter{
			HypothesisID: in.ID, Limit: in.Limit, Offset: in.Offset,
		})
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		jobCount, err := h.svc.Count(ctx, jobFilter)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		findings, findingCount, err := h.svc.ListHypothesisFindings(ctx, in.ID, in.Limit, in.Offset)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		comments, commentCount, err := h.svc.ListHypothesisComments(ctx, in.ID, in.Limit, in.Offset)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		return &struct{ Body hypothesisWithJobs }{Body: hypothesisWithJobs{
			Hypothesis: hyp,
			Jobs:       jobs, JobCount: jobCount,
			Findings: findings, FindingCount: findingCount,
			Comments: comments, CommentCount: commentCount,
		}}, nil
	})

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "add-hypothesis-comment", Method: "POST", Path: "/hypotheses/{id}/comments",
		Summary: "Add a comment to a hypothesis", Tags: []string{"hypotheses"},
		DefaultStatus: 201,
		Description: "A freeform note with no job behind it (abandon/revise/cross-reference), as opposed to " +
			"a finding, which requires a terminal job. Read the hypothesis's existing comments (GET " +
			"/hypotheses/{id}) first — don't re-record a conclusion already there.",
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
		OperationID: "set-hypothesis-status", Method: "POST", Path: "/hypotheses/{id}/status",
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
