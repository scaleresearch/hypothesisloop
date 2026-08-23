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
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/objectstore"
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
// Each of the three sub-lists GET /hypotheses/{id} returns is bounded like every other list read
// here — a small default for the agent that does not think about it, a ceiling for the one that
// asks. See db.defaultAgentListLimit for why the two differ.
const (
	defaultHypothesisDetailLimit = 20
	maxHypothesisDetailLimit     = 200
)

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
			if errors.Is(err, ErrExperimentNotFound) {
				return nil, huma.Error404NotFound(err.Error())
			}
			return nil, huma.Error500InternalServerError(err.Error())
		}
		return &struct{ Body []*domain.Experiment }{Body: chain}, nil
	})

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "list-experiment-data", Method: "GET", Path: "/experiments/{id}/data",
		Summary: "List what a job wrote to durable storage", Tags: []string{"experiments"},
		Description: "Checkpoints and datasets the job left under its own prefix, listed live from the object store. " +
			"A job that kept nothing lists as an empty array — that is the ordinary case, not an error. " +
			"Read any of them with the credentials in your own job's environment: HYPOTHESISLOOP_DATA_SHARED spans " +
			"the whole platform experiment, so any agent can load the bytes behind any claim.",
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*struct{ Body []objectstore.Object }, error) {
		objects, err := h.svc.ListData(ctx, in.ID)
		if err != nil {
			if errors.Is(err, ErrExperimentNotFound) {
				return nil, huma.Error404NotFound(err.Error())
			}
			return nil, huma.Error500InternalServerError(err.Error())
		}
		return &struct{ Body []objectstore.Object }{Body: objects}, nil
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
			if errors.Is(err, ErrExperimentNotFound) {
				return nil, huma.Error404NotFound(err.Error())
			}
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
			if errors.Is(err, ErrExperimentNotFound) {
				return nil, huma.Error404NotFound(err.Error())
			}
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
		N  int    `query:"n" default:"10" doc:"How many of the last reported lines to return. Capped by however many the owning cluster-agent last reported."`
	}) (*struct{ Body []string }, error) {
		lines, err := h.svc.GetLogTail(ctx, in.ID, in.N)
		if err != nil {
			if errors.Is(err, ErrExperimentNotFound) {
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
		MetricName    string  `query:"metric_name" doc:"Required. One of the metric keys this platform experiment declares (GET /platform-experiments/{id} -> metrics[].key)."`
		LookbackHours float64 `query:"lookback_hours" doc:"How far back to plot, default 24. Each series is downsampled to about 500 points regardless of the window, so a longer window costs resolution rather than response size."`
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
			"hypothesis from its own platform experiment. Exactly one of agent_id (an agent's row) " +
			"or author (a human's, submitted from the UI) must be set; both sit in the same pool " +
			"under the same dedup, and any agent may submit a job against either. What a human " +
			"author does not get is quota or a place in the standings — those key on agent_id, " +
			"and a job's quota and result belong to the agent that ran it.",
	}, func(ctx context.Context, in *struct {
		Body struct {
			// Neither is schema-required, and that is deliberate: exactly one of them must be
			// set, which is a rule about the PAIR that no per-field "required" can express.
			// Marking either one required rejects the other half of the very rule -- a required
			// author refuses every agent-submitted hypothesis, which is how every caller written
			// before humans could post one would break. domain.ClassifyHypothesisOrigin owns it.
			AgentID              string `json:"agent_id,omitempty" required:"false"`
			Author               string `json:"author,omitempty" required:"false" doc:"The name a human types in the UI. There is no auth: this is a claim, not an identity, exactly as agent_id is. Set this instead of agent_id, never both."`
			PlatformExperimentID string `json:"platform_experiment_id"`
			Text                 string `json:"text"`
		}
	}) (*struct {
		Status int
		Body   hypothesisResponse
	}, error) {
		b := in.Body
		if b.PlatformExperimentID == "" {
			return nil, huma.Error400BadRequest("platform_experiment_id is required")
		}
		if b.Text == "" {
			return nil, huma.Error400BadRequest("text is required")
		}
		hyp, existed, err := h.svc.RegisterHypothesis(ctx, b.AgentID, b.Author, b.PlatformExperimentID, b.Text)
		if err != nil {
			if errors.Is(err, domain.ErrHypothesisOrigin) {
				return nil, huma.Error400BadRequest(err.Error())
			}
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
			"for just that status, ?limit (default 20, max 200) and ?offset, most recent first. " +
			"Total match count is returned in the X-Total-Count response header. Rows carry " +
			"finding_count/comment_count — drill into GET /hypotheses/{id} only where a count or " +
			"relevance earns it.",
	}, func(ctx context.Context, in *struct {
		PlatformExperimentID string                  `query:"platform_experiment_id" doc:"Restrict to one platform experiment's idea pool. Omitted, every pool's hypotheses are candidates -- the operator-facing global view."`
		Agent                string                  `query:"agent" doc:"Only hypotheses registered by this agent."`
		Status               domain.HypothesisStatus `query:"status" doc:"open, confirmed, refuted or inconclusive -- the registering agent's own verdict on the claim, not job progress."`
		Limit                int                     `query:"limit" doc:"Page size, default 20, maximum 200. See X-Total-Count for how much you have not seen."`
		Offset               int                     `query:"offset" doc:"Rows to skip, for paging through X-Total-Count total matches."`
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
		Description: "?limit (default 20, max 200) and ?offset apply to each of jobs, findings and " +
			"comments alike, oldest first; job_count/finding_count/comment_count report the full " +
			"size of each set so a caller can page the rest.",
	}, func(ctx context.Context, in *struct {
		ID     string `path:"id"`
		Limit  int    `query:"limit" doc:"Page size applied to each of jobs, findings and comments alike; default 20, maximum 200. job_count/finding_count/comment_count report the full size of each set."`
		Offset int    `query:"offset" doc:"Rows to skip within each of the three lists."`
	}) (*struct{ Body hypothesisWithJobs }, error) {
		if in.Limit < 0 || in.Offset < 0 {
			return nil, huma.Error400BadRequest("limit and offset must not be negative")
		}
		if in.Limit <= 0 {
			in.Limit = defaultHypothesisDetailLimit
		} else if in.Limit > maxHypothesisDetailLimit {
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
			// See the register-hypothesis body above: exactly-one-of is a rule about the pair,
			// so neither field can be schema-required without rejecting the other's valid case.
			AgentID string `json:"agent_id,omitempty" required:"false"`
			Author  string `json:"author,omitempty" required:"false" doc:"The name a human types in the UI. Set this instead of agent_id, never both."`
			Text    string `json:"text"`
		}
	}) (*struct {
		Status int
		Body   *domain.HypothesisComment
	}, error) {
		if in.Body.Text == "" {
			return nil, huma.Error400BadRequest("text is required")
		}
		c, err := h.svc.AddHypothesisComment(ctx, in.ID, in.Body.AgentID, in.Body.Author, in.Body.Text)
		if err != nil {
			if errors.Is(err, domain.ErrHypothesisOrigin) {
				return nil, huma.Error400BadRequest(err.Error())
			}
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
			"into). Only the registering agent_id may set it; anyone else gets 403. A " +
			"human-submitted row has no registering agent, so any agent may settle it — an idea " +
			"nobody owns would otherwise sit open forever however much evidence it collected.",
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
