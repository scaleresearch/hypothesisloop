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
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/graphdb"
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
// equivalent row, and its outgoing links.
type hypothesisResponse struct {
	*domain.Hypothesis
	AlreadyExisted bool          `json:"already_existed"`
	Links          []domain.Link `json:"links"`
}

// hypothesisWithJobs bundles a hypothesis with its links, the jobs submitted against it and the
// findings and comments filed against it. Each of the three lists is one bounded page of a set
// that only ever grows, so each carries the full count it was drawn from — a caller can always
// tell a short list from a truncated one, and page the rest with ?limit/?offset.
type hypothesisWithJobs struct {
	*domain.Hypothesis
	Links        domain.HypothesisLinks      `json:"links"`
	Jobs         []*domain.Experiment        `json:"jobs"`
	JobCount     int                         `json:"job_count"`
	Findings     []*domain.HypothesisFinding `json:"findings"`
	FindingCount int                         `json:"finding_count"`
	Comments     []*domain.HypothesisComment `json:"comments"`
	CommentCount int                         `json:"comment_count"`
}

// hypothesisGraph is one platform experiment's whole hypothesis graph, rooted at its objective.
type hypothesisGraph struct {
	Root  string                       `json:"root"`
	Nodes []*domain.HypothesisListItem `json:"nodes"`
	Edges []domain.GraphEdge           `json:"edges"`
}

// similarHypothesesError is the 409 a registration gets while it reads close to hypotheses it has
// not been told apart from: the problem details plus the hypotheses themselves.
type similarHypothesesError struct {
	Status  int                        `json:"status"`
	Title   string                     `json:"title"`
	Detail  string                     `json:"detail"`
	Similar []domain.SimilarHypothesis `json:"similar"`
}

func (e *similarHypothesesError) Error() string  { return e.Detail }
func (e *similarHypothesesError) GetStatus() int { return e.Status }

const similarDetail = "Nothing was registered: your hypothesis reads close to the ones in `similar` " +
	"(reason close_text), or builds on a refuted one (refuted_parent). Read each. If one states the same claim as " +
	"yours, register nothing and test it under its id; only its author sets its status. A narrower, broader or " +
	"otherwise different claim is yours to register: resubmit naming each one, as a contradicts link when yours " +
	"cannot hold together with it, else in distinct_from (hl: --distinct-from), also when you build on it."

// hypothesisWriteError maps the errors a hypothesis write can return to the status an agent can
// act on, keeping the message.
func hypothesisWriteError(err error) error {
	var similar *graphdb.SimilarError
	if errors.As(err, &similar) {
		return &similarHypothesesError{Status: http.StatusConflict, Title: http.StatusText(http.StatusConflict),
			Detail: similarDetail, Similar: similar.Similar}
	}
	switch {
	case errors.Is(err, domain.ErrHypothesisOrigin), errors.Is(err, graphdb.ErrInvalidLink),
		errors.Is(err, ErrUnknownPlatformExperiment), errors.Is(err, db.ErrUnknownAgent):
		return huma.Error400BadRequest(err.Error())
	case errors.Is(err, ErrHypothesisNotOwner), errors.Is(err, ErrHypothesisSubmitterNotAllowed):
		return huma.Error403Forbidden(err.Error())
	case errors.Is(err, ErrHypothesisNotFound):
		return huma.Error404NotFound(err.Error())
	default:
		return huma.Error500InternalServerError(err.Error())
	}
}

// queryDescription is the schema an agent needs to write Cypher against the graph. It is served
// at /explore; an agent's prompt carries only /explore/rules.
var queryDescription = "Read-only Cypher against one platform experiment's hypothesis graph (Memgraph). " +
	"Body {platform_experiment_id, cypher, params}; answer " +
	"{columns, rows, truncated}. Use it to find where an idea belongs before registering it, and to walk lineage. " +
	"Schema: (:Objective {id}) is the root of one platform experiment, id = platform_experiment_id. " +
	"(:Hypothesis {id, platform_experiment_id, agent_id, source, author, text, status, created_at, status_changed_at}). " +
	"created_at and status_changed_at are datetimes, returned as RFC3339 text: compare them with " +
	"datetime('2026-10-02T01:30:00Z'), since a string never matches one. " +
	"Relationships, each {agent_id, author, created_at}: (:Hypothesis)-[:BUILDS_ON]->(:Hypothesis or :Objective) " +
	"(refines, extends, combines; acyclic, every hypothesis has one or more), (:Hypothesis)-[:CONTRADICTS]->(:Hypothesis), " +
	"(:Hypothesis)-[:RELATED]->(:Hypothesis). $pe is bound to platform_experiment_id: filter every MATCH and text search " +
	"with it; a result holding a node or relationship of another platform experiment is refused with 400. " +
	"$evidence is bound to a map by hypothesis id of what is outside the graph: {jobs (rejected aside), submitted, queued, " +
	"running, completed, failed, evicted, rejected, findings, comments} as counts and last_finding_at (datetime). A hypothesis with none " +
	"of these has no entry and reads as null: `WHERE coalesce($evidence[h.id].jobs, 0) = 0` finds the untested ones. " +
	"Full-text index " + graphdb.TextIndex + " over Hypothesis.text, ranked by how many of your words match: " +
	"`CALL text_search.search_all('" + graphdb.TextIndex + "', 'warmup cosine schedule') YIELD node, score " +
	"WHERE node.platform_experiment_id = $pe RETURN node.id, node.text, node.status, score ORDER BY score DESC LIMIT 10` " +
	"(plain words; quotes, colons and parentheses are search syntax). " +
	"Children of a hypothesis: `MATCH (c:Hypothesis)-[:BUILDS_ON]->(:Hypothesis {id: $id}) RETURN c.id, c.text, c.status`. " +
	"Nodes come back as their properties plus _labels, relationships as properties plus _type, _from and _to, paths as lists. " +
	"Nothing a query writes persists (writes go through POST /hypotheses and POST /hypotheses/{id}/links). " +
	"LOAD CSV, periodic commits and procedures other than text_search.* are refused. Limits: " +
	fmt.Sprintf("%s and %d MB of memory per query, %d rows (truncated=true when cut). ", graphdb.QueryTimeout, graphdb.QueryMemoryLimitMB, graphdb.QueryRowCap) +
	"A Cypher error comes back as 400 with Memgraph's message. Findings, comments and jobs are not in the graph: " +
	"GET /hypotheses/{id} returns them."

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
			"Read any of them from a job, or from your own shell after `eval \"$(hl data-key)\"`: " +
			"HYPOTHESISLOOP_DATA_SHARED spans the whole platform experiment.",
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
		if err := h.svc.RecordMetric(ctx, in.ID, in.Body.MetricName, in.Body.MetricBasis, in.Body.FractionComplete, in.Body.MetricValue); err != nil {
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
		Description: "One series per job so a dashboard can plot competing agents. Requires ?metric_name; optional ?lookback_hours (default 24) before ?end (default now).",
	}, func(ctx context.Context, in *struct {
		ID            string  `path:"id"`
		MetricName    string  `query:"metric_name" doc:"Required. One of the metric keys this platform experiment declares (GET /platform-experiments/{id} -> metrics[].key)."`
		LookbackHours float64 `query:"lookback_hours" doc:"How far back to plot, default 24. Each series is downsampled to about 500 points regardless of the window, so a longer window costs resolution rather than response size."`
		End           string  `query:"end" doc:"RFC3339. Where the window ends, default now; a time still ahead means now. Pass a finished platform experiment's ends_at so the window stays on its run."`
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
		var end time.Time
		if in.End != "" {
			parsed, err := time.Parse(time.RFC3339, in.End)
			if err != nil {
				return nil, huma.Error400BadRequest("end must be RFC3339")
			}
			end = parsed
		}
		series, err := h.svc.GetPlatformExperimentTimeseries(ctx, in.ID, in.MetricName, lookback, end)
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
		Summary: "Register a hypothesis in the graph", Tags: []string{"hypotheses"},
		DefaultStatus: 201,
		Description: "Every hypothesis has a place in its platform experiment's graph: links needs at least one " +
			"{type: builds_on, to} where to is an existing hypothesis id, or the platform_experiment_id itself for the " +
			"objective. Several builds_on make a combination of ideas; contradicts and related links may be added too. " +
			"Find where your idea belongs first with POST /hypotheses/query. Idempotent within a platform_experiment_id: " +
			"text equivalent to an existing hypothesis (modulo case/whitespace) returns that row with 200 and " +
			"already_existed=true and your links are not applied; a new one returns 201. While the text reads close to " +
			"hypotheses of the platform experiment it does not build on (word overlap), or a builds_on target is refuted, it answers 409 " +
			"with them in `similar` and registers nothing until each is named in distinct_from or in a contradicts or " +
			"related link. The returned id is " +
			"metadata.hypothesis_id for the job testing it; every job must reference a hypothesis from its own platform " +
			"experiment. Exactly one of agent_id (an agent's row) or author (a human's, submitted from the UI) must be set; " +
			"both sit in the same graph under the same dedup, and any agent may submit a job against either. What a human " +
			"author does not get is quota or a place in the standings — those key on agent_id.",
	}, func(ctx context.Context, in *struct {
		Body struct {
			// Neither is schema-required, and that is deliberate: exactly one of them must be
			// set, which is a rule about the PAIR that no per-field "required" can express.
			// domain.ClassifyHypothesisOrigin owns it.
			AgentID              string            `json:"agent_id,omitempty" required:"false"`
			Author               string            `json:"author,omitempty" required:"false" doc:"The name a human types in the UI. There is no auth: this is a claim, not an identity, exactly as agent_id is. Set this instead of agent_id, never both."`
			PlatformExperimentID string            `json:"platform_experiment_id"`
			Text                 string            `json:"text"`
			Links                []domain.LinkSpec `json:"links,omitempty" required:"false" doc:"Where the hypothesis sits: at least one {type: builds_on, to: <hypothesis id or the platform_experiment_id for the objective>}; contradicts and related links to hypotheses are optional."`
			DistinctFrom         []string          `json:"distinct_from,omitempty" required:"false" doc:"Ids of hypotheses a 409 listed that you read and judged a different claim from yours."`
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
		hyp, links, existed, err := h.svc.RegisterHypothesis(ctx, b.AgentID, b.Author, b.PlatformExperimentID, b.Text, b.Links, b.DistinctFrom)
		if err != nil {
			h.logger.Info("register hypothesis", zap.Error(err))
			return nil, hypothesisWriteError(err)
		}
		status := http.StatusCreated
		if existed {
			status = http.StatusOK
		}
		return &struct {
			Status int
			Body   hypothesisResponse
		}{Status: status, Body: hypothesisResponse{Hypothesis: hyp, AlreadyExisted: existed, Links: links}}, nil
	})

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "add-hypothesis-link", Method: "POST", Path: "/hypotheses/{id}/links",
		Summary: "Link a hypothesis to another", Tags: []string{"hypotheses"},
		DefaultStatus: 201,
		Description: "Adds one outgoing link: builds_on (to a hypothesis, or the platform_experiment_id for the " +
			"objective), contradicts or related (to a hypothesis in the same platform experiment). Only the registering " +
			"agent_id may link from its hypothesis; a human-submitted one can be linked by anyone, others get 403. " +
			"400 on a builds_on cycle, a self link, a target in another platform experiment or an unknown target. Links " +
			"are append-only and re-adding one changes nothing. Returns the hypothesis's outgoing links.",
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Body struct {
			AgentID string          `json:"agent_id,omitempty" required:"false"`
			Author  string          `json:"author,omitempty" required:"false" doc:"The name a human types in the UI. Set this instead of agent_id, never both."`
			Type    domain.LinkType `json:"type" doc:"builds_on, contradicts or related."`
			To      string          `json:"to" doc:"A hypothesis id, or the platform_experiment_id for the objective (builds_on only)."`
		}
	}) (*struct {
		Status int
		Body   []domain.Link
	}, error) {
		links, err := h.svc.AddHypothesisLink(ctx, in.ID, in.Body.AgentID, in.Body.Author, domain.LinkSpec{Type: in.Body.Type, To: in.Body.To})
		if err != nil {
			h.logger.Info("add hypothesis link", zap.Error(err))
			return nil, hypothesisWriteError(err)
		}
		return &struct {
			Status int
			Body   []domain.Link
		}{Status: http.StatusCreated, Body: links}, nil
	})

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "query-hypotheses", Method: "POST", Path: "/hypotheses/query",
		Summary: "Query the hypothesis graph with read-only Cypher", Tags: []string{"hypotheses"},
		Description: queryDescription,
	}, func(ctx context.Context, in *struct {
		Body struct {
			PlatformExperimentID string         `json:"platform_experiment_id" doc:"The platform experiment whose graph the query reads, bound as $pe."`
			Cypher               string         `json:"cypher"`
			Params               map[string]any `json:"params,omitempty" required:"false" doc:"Query parameters, referenced as $name; pe is reserved."`
		}
	}) (*struct{ Body *graphdb.QueryResult }, error) {
		if in.Body.PlatformExperimentID == "" || in.Body.Cypher == "" {
			return nil, huma.Error400BadRequest("platform_experiment_id and cypher are required")
		}
		result, err := h.svc.QueryGraph(ctx, in.Body.PlatformExperimentID, in.Body.Cypher, in.Body.Params)
		if err != nil {
			if errors.Is(err, graphdb.ErrInvalidQuery) {
				return nil, huma.Error400BadRequest(err.Error())
			}
			h.logger.Error("query hypotheses", zap.Error(err))
			return nil, huma.Error500InternalServerError(err.Error())
		}
		return &struct{ Body *graphdb.QueryResult }{Body: result}, nil
	})

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "get-hypothesis-graph", Method: "GET", Path: "/hypotheses/graph",
		Summary: "Get one platform experiment's whole hypothesis graph", Tags: []string{"hypotheses"},
		Description: "Every hypothesis of the platform experiment as a list item (nodes) and every link between them " +
			"(edges, {from, to, type, agent_id, author, created_at}); an edge to the objective has to = root = the platform_experiment_id. Unpaged, " +
			"for drawing the graph.",
	}, func(ctx context.Context, in *struct {
		PlatformExperimentID string `query:"platform_experiment_id" doc:"Required."`
	}) (*struct{ Body hypothesisGraph }, error) {
		if in.PlatformExperimentID == "" {
			return nil, huma.Error400BadRequest("platform_experiment_id is required")
		}
		nodes, edges, err := h.svc.HypothesisGraph(ctx, in.PlatformExperimentID)
		if errors.Is(err, ErrUnknownPlatformExperiment) {
			return nil, huma.Error404NotFound(err.Error())
		}
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		return &struct{ Body hypothesisGraph }{Body: hypothesisGraph{Root: in.PlatformExperimentID, Nodes: nodes, Edges: edges}}, nil
	})

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "list-hypotheses", Method: "GET", Path: "/hypotheses",
		Summary: "List hypotheses, optionally scoped to one platform experiment", Tags: []string{"hypotheses"},
		Description: "Optional ?platform_experiment_id restricts to one platform experiment; omitted, every " +
			"platform experiment's hypotheses are candidates (the operator-facing global view). " +
			"Optional ?agent for one agent's own, ?status (open|confirmed|refuted|inconclusive) " +
			"for just that status, ?limit (default 20, max 200) and ?offset, most recent first. " +
			"Total match count is returned in the X-Total-Count response header. Rows carry builds_on " +
			"(what each builds on; the platform_experiment_id stands for the objective), child_count " +
			"(how many build on it) and finding_count/comment_count. To walk or search the graph use " +
			"POST /hypotheses/query.",
	}, func(ctx context.Context, in *struct {
		PlatformExperimentID string                  `query:"platform_experiment_id" doc:"Restrict to one platform experiment. Omitted, every platform experiment's hypotheses are candidates -- the operator-facing global view."`
		Agent                string                  `query:"agent" doc:"Only hypotheses registered by this agent."`
		Status               domain.HypothesisStatus `query:"status" doc:"open, confirmed, refuted or inconclusive -- the registering agent's own verdict on the claim, not job progress."`
		Limit                int                     `query:"limit" doc:"Page size, default 20, maximum 200. See X-Total-Count for how much you have not seen."`
		Offset               int                     `query:"offset" doc:"Rows to skip, for paging through X-Total-Count total matches."`
	}) (*struct {
		Body       []*domain.HypothesisListItem
		TotalCount int `header:"X-Total-Count"`
	}, error) {
		if in.Status != "" && !domain.ValidHypothesisStatus(in.Status) {
			return nil, huma.Error400BadRequest(fmt.Sprintf("invalid status %q", in.Status))
		}
		if in.Limit < 0 || in.Offset < 0 {
			return nil, huma.Error400BadRequest("limit and offset must not be negative")
		}
		filter := graphdb.ListFilter{PlatformExperimentID: in.PlatformExperimentID, AgentID: in.Agent,
			Status: in.Status, Limit: in.Limit, Offset: in.Offset}
		hs, err := h.svc.ListHypotheses(ctx, filter)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		stats, err := h.svc.HypothesisStats(ctx, filter)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		return &struct {
			Body       []*domain.HypothesisListItem
			TotalCount int `header:"X-Total-Count"`
		}{Body: hs, TotalCount: stats.Total}, nil
	})

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "hypothesis-stats", Method: "GET", Path: "/hypotheses/stats",
		Summary: "Count hypotheses by status", Tags: []string{"hypotheses"},
		Description: "total and by_status over the hypotheses the ?platform_experiment_id and ?agent filters of " +
			"GET /hypotheses match. A status no hypothesis holds is absent.",
	}, func(ctx context.Context, in *struct {
		PlatformExperimentID string `query:"platform_experiment_id" doc:"Restrict to one platform experiment."`
		Agent                string `query:"agent" doc:"Only hypotheses registered by this agent."`
	}) (*struct{ Body *domain.HypothesisStats }, error) {
		stats, err := h.svc.HypothesisStats(ctx, graphdb.ListFilter{PlatformExperimentID: in.PlatformExperimentID, AgentID: in.Agent})
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		return &struct{ Body *domain.HypothesisStats }{Body: stats}, nil
	})

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "platform-experiment-findings", Method: "GET", Path: "/platform-experiments/{id}/findings",
		Summary: "What a platform experiment has found", Tags: []string{"hypotheses"},
		Description: "Among its 200 newest hypotheses, each one that holds a verdict, a finding or a job with a " +
			"metric, whatever its status, with finding, the newest one filed against it, and best_job {id, metric, summary}, " +
			"the job testing it with the best raw value on the first ranking metric and the summary filed for that job, if any. A finding whose created_at is " +
			"later than status_changed_at was filed after the verdict.",
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*struct{ Body []*HypothesisEvidence }, error) {
		findings, err := h.svc.Findings(ctx, in.ID)
		if errors.Is(err, ErrUnknownPlatformExperiment) {
			return nil, huma.Error404NotFound(err.Error())
		}
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		return &struct{ Body []*HypothesisEvidence }{Body: findings}, nil
	})

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "get-hypothesis", Method: "GET", Path: "/hypotheses/{id}",
		Summary: "Get a hypothesis with its links, jobs, findings, and comments", Tags: []string{"hypotheses"},
		Description: "links.out and links.in are its edges, oldest first, at most 200 each way. Each has type, the other " +
			"end's hypothesis_id, text and status (for the objective: the platform_experiment_id, empty text and status), " +
			"and the agent_id or author and created_at of whoever made the link. ?limit (default 20, max 200) and " +
			"?offset apply to each of jobs, findings and comments alike, oldest first; " +
			"job_count/finding_count/comment_count report the full size of each set so a caller can page the rest.",
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
		links, err := h.svc.GetHypothesisLinks(ctx, in.ID)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
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
			Links:      links,
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
			h.logger.Info("add hypothesis comment", zap.Error(err))
			return nil, hypothesisWriteError(err)
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
			h.logger.Info("set hypothesis status", zap.Error(err))
			return nil, hypothesisWriteError(err)
		}
		return &struct{ Body *domain.Hypothesis }{Body: hyp}, nil
	})
}
