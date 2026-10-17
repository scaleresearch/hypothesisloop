package registry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/graphdb"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/objectstore"
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
	// Similar is what a person's registration reads close to, given to them as a hint where an
	// agent's would be refused with 409 similar_hypotheses.
	Similar []domain.SimilarHypothesis `json:"similar" doc:"The hypotheses a signed-in person's new one reads close to, each with reason close_text or refuted_parent, as on a 409 similar_hypotheses. Empty for every other caller, who is refused instead."`
}

// hypothesisWithJobs bundles a hypothesis with its links, the jobs submitted against it and the
// findings and comments filed against it. Each of the three lists is one bounded page of a set
// that only ever grows, so each carries the full count it was drawn from — a caller can always
// tell a short list from a truncated one, and page the rest with ?limit/?offset.
type hypothesisWithJobs struct {
	*domain.Hypothesis
	Links domain.HypothesisLinks `json:"links"`
	// Implementations, Working and BaseRef are read off the jobs and checkouts on record.
	Implementations []domain.Implementation     `json:"implementations" doc:"The branches agents checked it out on or its jobs ran on, one per agent: agent_id, branch, code_ref, the commit the newest job on that branch ran, and checked_out_at."`
	Working         []domain.HypothesisWorker   `json:"working" doc:"The agents working on it now: a job of theirs queued or running, or a checkout within two hours."`
	BaseRef         string                      `json:"base_ref,omitempty" doc:"The commit a new implementation starts from, repository_url@sha: the one the newest completed job of its first builds_on parent ran, whichever agent's. A parent with no completed job passes the question to its own first parent, and the objective answers with the declared baseline job's commit. Absent while that chain ends at the objective and no baseline is declared."`
	Jobs            []*domain.Experiment        `json:"jobs"`
	JobCount        int                         `json:"job_count"`
	Findings        []*domain.HypothesisFinding `json:"findings"`
	FindingCount    int                         `json:"finding_count"`
	Comments        []*domain.HypothesisComment `json:"comments"`
	CommentCount    int                         `json:"comment_count"`
}

// similarHypothesesError is the 409 a registration gets while it reads close to hypotheses it has
// not been told apart from: the refusal with the hypotheses themselves beside it.
type similarHypothesesError struct {
	*refusal.Refusal
	Similar []domain.SimilarHypothesis `json:"similar"`
}

const similarDetail = "Nothing was registered: your hypothesis reads close to the ones in `similar` " +
	"(reason close_text), or builds on a refuted one (refuted_parent). Read each. If one states the same claim as " +
	"yours, register nothing and test it under its id; only its owner sets its status. A narrower, broader or " +
	"otherwise different claim is yours to register: resubmit naming each one, as a contradicts link when yours " +
	"cannot hold together with it, else in distinct_from (hl: --distinct-from ID). A refuted_parent stays your " +
	"parent: keep the builds_on link and name it in distinct_from when your claim changes what refuted it; do not " +
	"move your hypothesis onto the objective to get past this."

const closedDetail = "the platform experiment is closed, and its hypothesis graph takes no more hypotheses, links, verdicts or comments from anyone"

// closedRefusal is how each hypothesis write states its 409 on a closed platform experiment.
const closedRefusal = "409 experiment_closed once the platform experiment is closed, for every caller. "

// hypothesisWriteError maps the errors a hypothesis write can return to the status an agent can
// act on, keeping the message.
func (h *Handler) hypothesisWriteError(err error) error {
	var similar *graphdb.SimilarError
	if errors.As(err, &similar) {
		return &similarHypothesesError{Refusal: refusal.Refuse(http.StatusConflict, "similar_hypotheses", similarDetail), Similar: similar.Similar}
	}
	var unsupported *UnsupportedVerdictError
	if errors.As(err, &unsupported) {
		return huma.Error409Conflict(unsupported.Detail)
	}
	var inRecord *InRecordError
	if errors.As(err, &inRecord) {
		return refusal.Refuse(http.StatusConflict, "hypothesis_in_record", inRecord.Error())
	}
	switch {
	case errors.Is(err, graphdb.ErrDuplicateText):
		return refusal.Refuse(http.StatusConflict, "duplicate_hypothesis", "another hypothesis of this platform experiment already states this text: "+
			"nothing was changed. Test or comment on that one instead")
	case errors.Is(err, graphdb.ErrInvalidLink),
		errors.Is(err, ErrUnknownPlatformExperiment), errors.Is(err, db.ErrUnknownAgent):
		return huma.Error400BadRequest(err.Error())
	case errors.Is(err, ErrBaselineRegistersNoHypothesis):
		return refusal.Refuse(http.StatusForbidden, "baseline_registers_no_hypothesis", err.Error())
	case errors.Is(err, db.ErrPlatformExperimentClosed):
		return refusal.Refuse(http.StatusConflict, "experiment_closed", closedDetail)
	case errors.Is(err, db.ErrNotSignedUp):
		return refusal.Refuse(http.StatusForbidden, "not_signed_up", err.Error())
	case errors.Is(err, ErrHypothesisNotOwner):
		return huma.Error403Forbidden(err.Error())
	case errors.Is(err, ErrHypothesisNotFound):
		return huma.Error404NotFound(err.Error())
	default:
		return auth.Internal(h.logger, "write hypothesis", err)
	}
}

// queryDescription is the schema an agent needs to write Cypher against the graph. It is served
// at /explore; the instructions the platform serves an agent carry only the rules, as /explore/rules.
var queryDescription = "Read-only Cypher against one platform experiment's hypothesis graph (Memgraph). " +
	"Body {platform_experiment_id, cypher, params}; answer " +
	"{columns, rows, truncated}. Use it to find where an idea belongs before registering it, and to walk lineage. " +
	"Schema: (:Objective {id}) is the root of one platform experiment, id = platform_experiment_id. " +
	"(:Hypothesis {id, platform_experiment_id, agent_id, actor_kind, actor_id, text, status, created_at, status_changed_at, " +
	"status_actor_kind, status_actor_id}). agent_id is whose hypothesis it is, empty on one no agent owns. actor_kind " +
	"(user, key, agent, coordinator, staff) and actor_id are the credential that registered it, and status_actor_* the one " +
	"that gave the verdict; a node from before they were kept has neither, and author holds the name it stated. " +
	"created_at and status_changed_at are datetimes, returned as RFC3339 text: compare them with " +
	"datetime('2026-10-02T01:30:00Z'), since a string never matches one. " +
	"Relationships, each {agent_id, actor_kind, actor_id, created_at}: (:Hypothesis)-[:BUILDS_ON]->(:Hypothesis or :Objective) " +
	"(refines, extends, combines; acyclic, every hypothesis has one or more), (:Hypothesis)-[:CONTRADICTS]->(:Hypothesis), " +
	"(:Hypothesis)-[:RELATED]->(:Hypothesis). A query reads this platform experiment and nothing else, whatever it returns: " +
	"the platform rewrites it before it runs. Every node pattern names its label, (h:Hypothesis) or (o:Objective), and is " +
	"tied to $pe (platform_experiment_id on a hypothesis, id on the objective), which you may leave out or write as " +
	"{platform_experiment_id: $pe} and {id: $pe}; a variable-length relationship ([:BUILDS_ON*1..3]) stays inside the " +
	"platform experiment and takes no filter of its own; a text search keeps this platform experiment's hits. Refused with 400: " +
	"a node without a label, as (n), () or a variable in parentheses on its own, which is then written (n:Hypothesis) " +
	"again, another or a second label, the scope property set to anything but $pe, a parameter as a property map, a " +
	"function outside the built-in ones (" + strings.Join(graphdb.QueryFunctions, ", ") + "), a statement that does not start with " +
	"MATCH, OPTIONAL MATCH, WITH, UNWIND, RETURN or CALL, and a CALL without YIELD node. " +
	"$evidence is bound to a map by hypothesis id of what is outside the graph: {jobs (rejected aside), submitted, queued, " +
	"running, completed, failed, evicted, rejected, findings, comments} as counts and last_finding_at (datetime). A hypothesis with none " +
	"of these has no entry and reads as null: `WHERE coalesce($evidence[h.id].jobs, 0) = 0` finds the untested ones. " +
	"$evidence[$pe] is the objective's: the baseline's jobs and the comments on the objective. " +
	"Full-text index " + graphdb.TextIndex + " over Hypothesis.text, ranked by how many of your words match: " +
	"`CALL text_search.search_all('" + graphdb.TextIndex + "', 'warmup cosine schedule') YIELD node, score " +
	"RETURN node.id, node.text, node.status, score ORDER BY score DESC LIMIT 10` " +
	"(plain words; quotes, colons and parentheses are search syntax). " +
	"Children of a hypothesis: `MATCH (c:Hypothesis)-[:BUILDS_ON]->(:Hypothesis {id: $id}) RETURN c.id, c.text, c.status`. " +
	"Nodes come back as their properties plus _labels, relationships as properties plus _type, _from and _to, paths as lists. " +
	"Nothing a query writes persists (writes go through POST /hypotheses and POST /hypotheses/{id}/links). " +
	"LOAD CSV, periodic commits and procedures other than " + strings.Join(graphdb.QueryProcedures, ", ") + " are refused. Limits: " +
	fmt.Sprintf("%s and %d MB of memory per query, %d rows (truncated=true when cut). ", graphdb.QueryTimeout, graphdb.QueryMemoryLimitMB, graphdb.QueryRowCap) +
	"A Cypher error comes back as 400 with Memgraph's message. Findings, comments and jobs are not in the graph: " +
	"GET /hypotheses/{id} returns them, and GET /hypotheses/{platform_experiment_id}/comments the objective's comments."

// RegisterHuma registers the registry's operations: what hangs off an experiment (metrics,
// logs) and the hypothesis notebook. Paths sit under the resource they belong to —
// /experiments/{id}/... and /hypotheses/... — so a caller never has to know which service
// implements them; they are all registered on the one API doc alongside scheduler and quota.
// Each of the three sub-lists GET /hypotheses/{id} returns is bounded like every other list read
// here — a small default for the agent that does not think about it, a ceiling for the one that
// asks. See db.defaultAgentListLimit for why the two differ.
const (
	defaultPageLimit = 20
	maxPageLimit     = 200
)

// page returns the bounds of one page of a hypothesis listing. A limit over the maximum is
// refused, so a short page is never read as the whole set.
func page(limit, offset int) (int, int, error) {
	if limit < 0 || limit > maxPageLimit || offset < 0 {
		return 0, 0, refusal.Refuse(http.StatusUnprocessableEntity, "invalid_page", fmt.Sprintf("limit must be 1 to %d and offset must not be negative", maxPageLimit))
	}
	if limit == 0 {
		return defaultPageLimit, offset, nil
	}
	return limit, offset, nil
}

func RegisterHuma(doc *apidocs.Doc, h *Handler) {
	registerCheckoutHuma(doc, h)
	access := doc.Access
	// loadHypothesis returns a hypothesis the caller reaches. One it does not reach and one that
	// does not exist are the same answer.
	loadHypothesis := func(ctx context.Context, id string) (*domain.Hypothesis, error) {
		hyp, err := h.svc.GetHypothesis(ctx, id)
		if err != nil {
			return nil, auth.Internal(h.logger, "get hypothesis", err)
		}
		if hyp == nil {
			return nil, auth.NotFound("hypothesis")
		}
		return hyp, access.Within(ctx, "hypothesis", hyp.PlatformExperimentID)
	}
	// commentsOf admits the caller to the comments of id: a hypothesis it reaches, or the
	// objective of a platform experiment it reaches, named by the platform experiment's id.
	commentsOf := func(ctx context.Context, id string) error {
		hyp, err := h.svc.GetHypothesis(ctx, id)
		if err != nil {
			return auth.Internal(h.logger, "get hypothesis", err)
		}
		if hyp != nil {
			return access.Within(ctx, "hypothesis", hyp.PlatformExperimentID)
		}
		if err := access.Within(ctx, "hypothesis", id); err != nil {
			return err
		}
		if _, err := h.svc.CommentTarget(ctx, id); errors.Is(err, ErrHypothesisNotFound) {
			return auth.NotFound("hypothesis")
		} else if err != nil {
			return auth.Internal(h.logger, "get hypothesis", err)
		}
		return nil
	}
	// referencesInScope admits the caller to everything a write names as a link target or in
	// distinct_from: a hypothesis it reaches, or its platform experiment's id for the objective.
	// The service's own refusal for a target elsewhere names that platform experiment.
	referencesInScope := func(ctx context.Context, ids ...string) error {
		for _, id := range ids {
			hyp, err := h.svc.GetHypothesis(ctx, id)
			if err != nil {
				return auth.Internal(h.logger, "get hypothesis", err)
			}
			placedIn := id
			if hyp != nil {
				placedIn = hyp.PlatformExperimentID
			}
			if err := access.Within(ctx, "hypothesis", placedIn); err != nil {
				return err
			}
		}
		return nil
	}
	// ---- what hangs off an experiment ----

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "list-experiment-data", Method: "GET", Path: "/experiments/{id}/data",
		Summary: "List what a job wrote to durable storage", Tags: []string{"experiments"},
		Description: "Checkpoints and datasets the job left under its own prefix, listed live from the object store. " +
			"A job that kept nothing lists as an empty array — that is the ordinary case, not an error. " +
			"Read any of them from a job, or from your own shell after `eval \"$(hl data-key)\"`: " +
			"HYPOTHESISLOOP_DATA_SHARED spans the whole platform experiment. Each object carries a url a plain GET reads it by for 12 hours, " +
			"with no key: how the data of a job of a prior run is read, since no data key reaches another run.",
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*struct{ Body []objectstore.Object }, error) {
		if err := access.Experiment(ctx, in.ID); err != nil {
			return nil, err
		}
		objects, err := h.svc.ListData(ctx, in.ID)
		if err != nil {
			if errors.Is(err, ErrExperimentNotFound) {
				return nil, huma.Error404NotFound(err.Error())
			}
			return nil, auth.Internal(h.logger, "list experiment data", err, zap.String("id", in.ID))
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
		if err := access.Experiment(ctx, in.ID); err != nil {
			return nil, err
		}
		ts, err := h.svc.GetTimeseries(ctx, in.ID)
		if err != nil {
			if errors.Is(err, ErrExperimentNotFound) {
				return nil, huma.Error404NotFound(err.Error())
			}
			return nil, auth.Internal(h.logger, "get metrics", err, zap.String("id", in.ID))
		}
		return &struct{ Body []*domain.MetricDataPoint }{Body: ts}, nil
	})

	apidocs.Register(doc, apidocs.AudienceJob, huma.Operation{
		OperationID: "append-metric", Method: "POST", Path: "/experiments/{id}/metrics",
		Summary: "Append a metric sample for an experiment", Tags: []string{"metrics"},
		DefaultStatus: 204,
		Description: "Records one {metric_name, fraction_complete, metric_value} sample. No server-side validation of the value — never fabricate one. " +
			"A job sends the token it finds in HYPOTHESISLOOP_TOKEN, which is accepted on this call for that job only and until it ends. " +
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
		if err := access.OwnExperiment(ctx, in.ID); err != nil {
			return nil, err
		}
		if err := h.svc.RecordMetric(ctx, in.ID, in.Body.MetricName, in.Body.MetricBasis, in.Body.FractionComplete, in.Body.MetricValue); err != nil {
			if errors.Is(err, ErrExperimentNotFound) {
				return nil, huma.Error404NotFound(err.Error())
			}
			if errors.Is(err, ErrInvalidMetric) {
				return nil, huma.Error422UnprocessableEntity(err.Error())
			}
			return nil, auth.Internal(h.logger, "append metric", err, zap.String("id", in.ID))
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
		if err := access.Experiment(ctx, in.ID); err != nil {
			return nil, err
		}
		lines, err := h.svc.GetLogTail(ctx, in.ID, in.N)
		if err != nil {
			if errors.Is(err, ErrExperimentNotFound) {
				return nil, huma.Error404NotFound(err.Error())
			}
			return nil, auth.Internal(h.logger, "get log tail", err, zap.String("id", in.ID))
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
		if err := access.PlatformExperiment(ctx, in.ID); err != nil {
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
		series, err := h.svc.GetPlatformExperimentTimeseries(ctx, in.ID, in.MetricName, lookback, end)
		if err != nil {
			return nil, auth.Internal(h.logger, "platform experiment timeseries", err, zap.String("id", in.ID))
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

	// The hypothesis writes are AfterClose so that a coordinator token is refused like every other
	// caller, by the store's 409 experiment_closed, and not first by the gate's 403.
	apidocs.Register(doc, apidocs.AudienceAgent, apidocs.AfterClose(huma.Operation{
		OperationID: "register-hypothesis", Method: "POST", Path: "/hypotheses",
		Summary: "Register a hypothesis in the graph", Tags: []string{"hypotheses"},
		DefaultStatus: 201,
		Description: "Every hypothesis has a place in its platform experiment's graph: links needs at least one " +
			"{type: builds_on, to} where to is an existing hypothesis id, or the platform_experiment_id itself for the " +
			"objective. Several builds_on make a combination of ideas; contradicts and related links may be added too. " +
			"Find where your idea belongs first with POST /hypotheses/query. Idempotent within a platform_experiment_id: " +
			"text equivalent to an existing hypothesis (modulo case/whitespace) returns that row with 200 and " +
			"already_existed=true and your links are not applied; a new one returns 201. While the text reads close to " +
			"hypotheses of the platform experiment it does not build on (word overlap, at a lower bar for one that builds on the objective alone), or a builds_on target is refuted, it answers 409 similar_hypotheses " +
			"with them in `similar` and registers nothing until each is named in distinct_from or in a contradicts or " +
			"related link. A signed-in person is not refused: their hypothesis is registered with 201 and the ones it reads close to come back in similar, as a hint. The returned id is " +
			"metadata.hypothesis_id for the job testing it; every job must reference a hypothesis from its own platform " +
			"experiment. " +
			"Who registered it is the caller, answered as actor, and no field of the body names it: author is refused. " +
			"agent_id says whose hypothesis it is: an agent token's is its own agent's, named or not, and any other caller names an agent it " +
			"reaches to register for it, or names none and the hypothesis has no owning agent. Both sit in the same graph under the same " +
			"dedup, and any agent may submit a job against either. 403 not_signed_up for an agent that has no place in this platform experiment. 403 baseline_registers_no_hypothesis for an agent of role baseline: the baseline " +
			"is the objective's own measurement and its job names no hypothesis. " + closedRefusal +
			"What one with no owning agent does not get is quota or a place in the " +
			"standings — those key on agent_id.",
	}), func(ctx context.Context, in *struct {
		Body struct {
			AgentID              string            `json:"agent_id,omitempty" required:"false" doc:"The agent whose hypothesis this is. Absent, it is the caller's own: an agent token's agent, and for any other caller no agent."`
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
		if err := access.PlatformExperiment(ctx, b.PlatformExperimentID); err != nil {
			return nil, err
		}
		owner, actor, err := access.Writer(ctx, b.AgentID)
		if err != nil {
			return nil, err
		}
		referenced := append([]string{}, b.DistinctFrom...)
		for _, link := range b.Links {
			referenced = append(referenced, link.To)
		}
		if err := referencesInScope(ctx, referenced...); err != nil {
			return nil, err
		}
		hyp, links, existed, similar, err := h.svc.RegisterHypothesis(ctx, owner, actor, b.PlatformExperimentID, b.Text, b.Links, b.DistinctFrom)
		if err != nil {
			h.logger.Info("register hypothesis", zap.Error(err))
			return nil, h.hypothesisWriteError(err)
		}
		status := http.StatusCreated
		if existed {
			status = http.StatusOK
		}
		return &struct {
			Status int
			Body   hypothesisResponse
		}{Status: status, Body: hypothesisResponse{Hypothesis: hyp, AlreadyExisted: existed, Links: links, Similar: append([]domain.SimilarHypothesis{}, similar...)}}, nil
	})

	apidocs.Register(doc, apidocs.AudienceAgent, apidocs.AfterClose(huma.Operation{
		OperationID: "add-hypothesis-link", Method: "POST", Path: "/hypotheses/{id}/links",
		Summary: "Link a hypothesis to another", Tags: []string{"hypotheses"},
		DefaultStatus: 201,
		Description: "Adds one outgoing link: builds_on (to a hypothesis, or the platform_experiment_id for the " +
			"objective), contradicts or related (to a hypothesis in the same platform experiment). Only the agent whose " +
			"hypothesis it is may link from it, and one with no owning agent can be linked by anyone; others get 403. " +
			"agent_id says whose link it is, as on registering, and who made it is the caller, answered as actor: author is refused. " +
			"400 on a builds_on cycle, a self link, a target in another platform experiment or an unknown target. Links " +
			"are append-only and re-adding one changes nothing. " + closedRefusal + "Returns the hypothesis's outgoing links.",
	}), func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Body struct {
			AgentID string          `json:"agent_id,omitempty" required:"false" doc:"The agent whose link this is. Absent, it is the caller's own: an agent token's agent, and for any other caller no agent."`
			Type    domain.LinkType `json:"type" doc:"builds_on, contradicts or related."`
			To      string          `json:"to" doc:"A hypothesis id, or the platform_experiment_id for the objective (builds_on only)."`
		}
	}) (*struct {
		Status int
		Body   []domain.Link
	}, error) {
		if _, err := loadHypothesis(ctx, in.ID); err != nil {
			return nil, err
		}
		if err := referencesInScope(ctx, in.Body.To); err != nil {
			return nil, err
		}
		owner, actor, err := access.Writer(ctx, in.Body.AgentID)
		if err != nil {
			return nil, err
		}
		links, err := h.svc.AddHypothesisLink(ctx, in.ID, owner, actor, domain.LinkSpec{Type: in.Body.Type, To: in.Body.To})
		if err != nil {
			h.logger.Info("add hypothesis link", zap.Error(err))
			return nil, h.hypothesisWriteError(err)
		}
		return &struct {
			Status int
			Body   []domain.Link
		}{Status: http.StatusCreated, Body: links}, nil
	})

	apidocs.Register(doc, apidocs.AudienceAgent, apidocs.Read(apidocs.AfterClose(huma.Operation{
		OperationID: "query-hypotheses", Method: "POST", Path: "/hypotheses/query",
		Summary: "Query the hypothesis graph with read-only Cypher", Tags: []string{"hypotheses"},
		Description: queryDescription,
	})), func(ctx context.Context, in *struct {
		Body struct {
			PlatformExperimentID string         `json:"platform_experiment_id" doc:"The platform experiment whose graph the query reads, bound as $pe."`
			Cypher               string         `json:"cypher"`
			Params               map[string]any `json:"params,omitempty" required:"false" doc:"Query parameters, referenced as $name; pe and evidence are bound by the platform and refused here."`
		}
	}) (*struct{ Body *graphdb.QueryResult }, error) {
		if in.Body.PlatformExperimentID == "" || in.Body.Cypher == "" {
			return nil, huma.Error400BadRequest("platform_experiment_id and cypher are required")
		}
		if err := access.PlatformExperiment(ctx, in.Body.PlatformExperimentID); err != nil {
			return nil, err
		}
		result, err := h.svc.QueryGraph(ctx, in.Body.PlatformExperimentID, in.Body.Cypher, in.Body.Params)
		if err != nil {
			if errors.Is(err, graphdb.ErrInvalidQuery) {
				return nil, huma.Error400BadRequest(err.Error())
			}
			return nil, auth.Internal(h.logger, "query hypotheses", err)
		}
		return &struct{ Body *graphdb.QueryResult }{Body: result}, nil
	})

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "get-hypothesis-graph", Method: "GET", Path: "/hypotheses/graph",
		Summary: "Get one platform experiment's whole hypothesis graph", Tags: []string{"hypotheses"},
		Description: "Every hypothesis of the platform experiment as a list item (nodes) and every link between them " +
			"(edges, {from, to, type, agent_id, actor, created_at}); an edge to the objective has to = root = the platform_experiment_id. " +
			"A node also carries finding, the newest one filed against it, and best_job {id, metric, summary}, the completed job testing it with the " +
			"best raw value on the first ranking metric and the summary filed for that job; each is absent when there is none. " +
			"objective carries the same two for the root: the jobs of the baseline agent test no hypothesis and hang on the objective. " +
			"It also carries comment_count, the comments on the objective, and comments, the newest five of them, oldest first. Unpaged, " +
			"for drawing the graph.",
	}, func(ctx context.Context, in *struct {
		PlatformExperimentID string `query:"platform_experiment_id" doc:"Required."`
	}) (*struct{ Body *HypothesisGraph }, error) {
		if in.PlatformExperimentID == "" {
			return nil, huma.Error400BadRequest("platform_experiment_id is required")
		}
		if err := access.PlatformExperiment(ctx, in.PlatformExperimentID); err != nil {
			return nil, err
		}
		graph, err := h.svc.HypothesisGraph(ctx, in.PlatformExperimentID)
		if errors.Is(err, ErrUnknownPlatformExperiment) {
			return nil, huma.Error404NotFound(err.Error())
		}
		if err != nil {
			return nil, auth.Internal(h.logger, "get hypothesis graph", err)
		}
		return &struct{ Body *HypothesisGraph }{Body: graph}, nil
	})

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "list-hypotheses", Method: "GET", Path: "/hypotheses",
		Summary: "List hypotheses, optionally scoped to one platform experiment", Tags: []string{"hypotheses"},
		Description: "Optional ?platform_experiment_id restricts to one platform experiment; omitted, every " +
			"platform experiment's hypotheses are candidates (the operator-facing global view). " +
			"Optional ?agent_id for one agent's own, ?status (open|confirmed|refuted|inconclusive) " +
			"for just that status, ?by=people for the ideas people registered, ?q for those whose text holds it, ignoring case, ?untaken=true for those no agent works on now (see working), ?limit (default 20, max 200, 422 invalid_page over it) and ?offset. " +
			"The open ideas of people come first, those no agent has settled or built on yet, then the most recent. " +
			"Total match count is returned in the X-Total-Count response header. Rows carry builds_on " +
			"(what each builds on; the platform_experiment_id stands for the objective), child_count " +
			"(how many build on it), finding_count/comment_count, finding and best_job as GET /hypotheses/graph gives them, and working, the agents on it now: a job of theirs queued or running, or a " +
			"checkout within two hours, absent while none is. To walk or search the graph use " +
			"POST /hypotheses/query.",
	}, func(ctx context.Context, in *struct {
		PlatformExperimentID string                  `query:"platform_experiment_id" doc:"Restrict to one platform experiment. Omitted, every platform experiment's hypotheses are candidates -- the operator-facing global view."`
		ProjectID            string                  `query:"project_id" doc:"Only hypotheses of this project's platform experiments."`
		Agent                string                  `query:"agent_id" doc:"Only hypotheses registered by this agent."`
		Status               domain.HypothesisStatus `query:"status" doc:"open, confirmed, refuted or inconclusive -- the registering agent's own verdict on the claim, not job progress."`
		By                   string                  `query:"by" enum:"people" doc:"people: only the hypotheses a person registered, an API key or staff included."`
		Search               string                  `query:"q" doc:"Only hypotheses whose text holds this, ignoring case."`
		Untaken              bool                    `query:"untaken" doc:"true: only the hypotheses no agent works on now: no job of theirs queued or running and no checkout within two hours."`
		Limit                int                     `query:"limit" doc:"Page size, default 20, maximum 200. A larger one is refused. See X-Total-Count for how much you have not seen."`
		Offset               int                     `query:"offset" doc:"Rows to skip, for paging through X-Total-Count total matches."`
	}) (*struct {
		Body       []*GraphNode
		TotalCount int `header:"X-Total-Count"`
	}, error) {
		if in.Status != "" && !domain.ValidHypothesisStatus(in.Status) {
			return nil, huma.Error400BadRequest(fmt.Sprintf("invalid status %q", in.Status))
		}
		limit, offset, err := page(in.Limit, in.Offset)
		if err != nil {
			return nil, err
		}
		within, restricted, err := access.PlatformExperimentIDs(ctx, in.ProjectID)
		if err != nil {
			return nil, err
		}
		filter := graphdb.ListFilter{PlatformExperimentID: in.PlatformExperimentID, AgentID: in.Agent,
			Restricted: restricted, Within: within, Status: in.Status, ByPeople: in.By == "people", Search: in.Search, Limit: limit, Offset: offset}
		if in.Untaken {
			if filter.ExcludeIDs, err = h.svc.TakenHypotheses(ctx, in.PlatformExperimentID); err != nil {
				return nil, auth.Internal(h.logger, "list hypotheses", err)
			}
		}
		hs, err := h.svc.ListHypotheses(ctx, filter)
		if err != nil {
			return nil, auth.Internal(h.logger, "list hypotheses", err)
		}
		stats, err := h.svc.HypothesisStats(ctx, filter)
		if err != nil {
			return nil, auth.Internal(h.logger, "list hypotheses", err)
		}
		return &struct {
			Body       []*GraphNode
			TotalCount int `header:"X-Total-Count"`
		}{Body: hs, TotalCount: stats.Total}, nil
	})

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "hypothesis-stats", Method: "GET", Path: "/hypotheses/stats",
		Summary: "Count hypotheses by status", Tags: []string{"hypotheses"},
		Description: "total and by_status over the hypotheses the ?platform_experiment_id and ?agent_id filters of " +
			"GET /hypotheses match. A status no hypothesis holds is absent.",
	}, func(ctx context.Context, in *struct {
		PlatformExperimentID string `query:"platform_experiment_id" doc:"Restrict to one platform experiment."`
		ProjectID            string `query:"project_id" doc:"Only hypotheses of this project's platform experiments."`
		Agent                string `query:"agent_id" doc:"Only hypotheses registered by this agent."`
	}) (*struct{ Body *domain.HypothesisStats }, error) {
		within, restricted, err := access.PlatformExperimentIDs(ctx, in.ProjectID)
		if err != nil {
			return nil, err
		}
		stats, err := h.svc.HypothesisStats(ctx, graphdb.ListFilter{PlatformExperimentID: in.PlatformExperimentID, AgentID: in.Agent,
			Restricted: restricted, Within: within})
		if err != nil {
			return nil, auth.Internal(h.logger, "hypothesis stats", err)
		}
		return &struct{ Body *domain.HypothesisStats }{Body: stats}, nil
	})

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "platform-experiment-findings", Method: "GET", Path: "/platform-experiments/{id}/findings",
		Summary: "What a platform experiment has found", Tags: []string{"hypotheses"},
		Description: "Among its 200 newest hypotheses, each one that holds a verdict, a finding or a job with a " +
			"metric, whatever its status, with finding, the newest one filed against it, and best_job {id, metric, summary}, " +
			"the completed job testing it with the best raw value on the first ranking metric and the summary filed for that job, if any. A finding whose created_at is " +
			"later than status_changed_at was filed after the verdict.",
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*struct{ Body []*HypothesisEvidence }, error) {
		if err := access.PlatformExperiment(ctx, in.ID); err != nil {
			return nil, err
		}
		findings, err := h.svc.Findings(ctx, in.ID)
		if errors.Is(err, ErrUnknownPlatformExperiment) {
			return nil, huma.Error404NotFound(err.Error())
		}
		if err != nil {
			return nil, auth.Internal(h.logger, "platform experiment findings", err, zap.String("id", in.ID))
		}
		return &struct{ Body []*HypothesisEvidence }{Body: findings}, nil
	})

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "get-hypothesis", Method: "GET", Path: "/hypotheses/{id}",
		Summary: "Get a hypothesis with its links, jobs, findings, and comments", Tags: []string{"hypotheses"},
		Description: "links.out and links.in are its edges, oldest first, at most 200 each way. Each has type, the other " +
			"end's hypothesis_id, text and status (for the objective: the platform_experiment_id, empty text and status), " +
			"and the agent_id the link was made for, the actor that made it and its created_at. " +
			"A hypothesis and a branch are two things: any agent may run its own implementation of any hypothesis, and implementations lists " +
			"them as checkouts and jobs show them, {agent_id, branch, code_ref, checked_out_at}, code_ref being the commit the newest job on that branch ran, empty before one. " +
			"working is the agents on it now: a job of theirs queued or running, or a checkout within two hours. " +
			"base_ref is the commit a new implementation starts from: the one the newest completed job of the first builds_on parent ran, " +
			"that parent's own base_ref while it has no completed job, and the declared baseline job's commit where the chain ends at the objective. ?limit (default 20, max 200, 422 invalid_page over it) and " +
			"?offset apply to each of jobs, findings and comments alike, oldest first; " +
			"job_count/finding_count/comment_count report the full size of each set so a caller can page the rest.",
	}, func(ctx context.Context, in *struct {
		ID     string `path:"id"`
		Limit  int    `query:"limit" doc:"Page size applied to each of jobs, findings and comments alike; default 20, maximum 200. A larger one is refused. job_count/finding_count/comment_count report the full size of each set."`
		Offset int    `query:"offset" doc:"Rows to skip within each of the three lists."`
	}) (*struct{ Body hypothesisWithJobs }, error) {
		var err error
		if in.Limit, in.Offset, err = page(in.Limit, in.Offset); err != nil {
			return nil, err
		}
		hyp, err := loadHypothesis(ctx, in.ID)
		if err != nil {
			if commentsOf(ctx, in.ID) == nil {
				return nil, huma.Error404NotFound(fmt.Sprintf("%s is the objective, no hypothesis: GET /hypotheses/%s/comments lists the comments on it, and its jobs are the baseline's", in.ID, in.ID))
			}
			return nil, err
		}
		if err := h.svc.NameHypothesis(ctx, hyp); err != nil {
			return nil, auth.Internal(h.logger, "name the hypothesis's actors", err)
		}
		links, err := h.svc.GetHypothesisLinks(ctx, in.ID)
		if err != nil {
			return nil, auth.Internal(h.logger, "get hypothesis", err, zap.String("id", in.ID))
		}
		implementations, err := h.svc.Implementations(ctx, in.ID)
		if err != nil {
			return nil, auth.Internal(h.logger, "get hypothesis", err, zap.String("id", in.ID))
		}
		working, err := h.svc.Working(ctx, []string{in.ID})
		if err != nil {
			return nil, auth.Internal(h.logger, "get hypothesis", err, zap.String("id", in.ID))
		}
		baseRef, err := h.svc.BaseRef(ctx, hyp)
		if err != nil {
			return nil, auth.Internal(h.logger, "get hypothesis", err, zap.String("id", in.ID))
		}
		jobFilter := domain.ExperimentFilter{Scope: domain.AllTenants, HypothesisID: in.ID}
		jobs, err := h.svc.List(ctx, domain.ExperimentFilter{
			Scope: domain.AllTenants, HypothesisID: in.ID, Limit: in.Limit, Offset: in.Offset,
		})
		if err != nil {
			return nil, auth.Internal(h.logger, "get hypothesis", err, zap.String("id", in.ID))
		}
		jobCount, err := h.svc.Count(ctx, jobFilter)
		if err != nil {
			return nil, auth.Internal(h.logger, "get hypothesis", err, zap.String("id", in.ID))
		}
		findings, findingCount, err := h.svc.ListHypothesisFindings(ctx, in.ID, in.Limit, in.Offset)
		if err != nil {
			return nil, auth.Internal(h.logger, "get hypothesis", err, zap.String("id", in.ID))
		}
		comments, commentCount, err := h.svc.ListHypothesisComments(ctx, in.ID, in.Limit, in.Offset)
		if err != nil {
			return nil, auth.Internal(h.logger, "get hypothesis", err, zap.String("id", in.ID))
		}
		return &struct{ Body hypothesisWithJobs }{Body: hypothesisWithJobs{
			Hypothesis: hyp,
			Links:      links, Implementations: implementations, Working: append([]domain.HypothesisWorker{}, working[in.ID]...), BaseRef: baseRef,
			Jobs: jobs, JobCount: jobCount,
			Findings: findings, FindingCount: findingCount,
			Comments: comments, CommentCount: commentCount,
		}}, nil
	})

	// changer is who asks to change hyp, as ChangeRefusal reads them. Whether they own its org is
	// read only for a removal they did not write.
	changer := func(ctx context.Context, hyp *domain.Hypothesis, remove bool) (Changer, error) {
		p, _ := auth.FromContext(ctx)
		c := Changer{UserID: p.UserID}
		if remove && c.UserID != "" && hyp.Actor.ID != c.UserID {
			var err error
			if c.OrgOwner, err = access.OwnsOrgOf(ctx, hyp.PlatformExperimentID); err != nil {
				return Changer{}, auth.Internal(h.logger, "read the caller's org role", err)
			}
		}
		return c, nil
	}

	const changeRule = "Only for an idea a person wrote for no agent, and only while no agent has acted on it: no job tests it " +
		"and no other hypothesis links to it. Once one has, it is part of the record and the answer is 409 hypothesis_in_record " +
		"naming the jobs and the links; add a comment instead. 403 person_only for a request no signed-in person makes, " +
		"agent_hypothesis for an agent's hypothesis, not_a_persons_idea for one registered with an API key, a coordinator's or " +
		"staff's token, and not_the_author for anyone else. 409 experiment_closed once the platform experiment is closed."

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "edit-hypothesis", Method: "PATCH", Path: "/hypotheses/{id}",
		Summary: "Edit the text of an idea you wrote", Tags: []string{"hypotheses"},
		Description: "Replaces text, by the person who wrote the idea. " + changeRule + " 409 duplicate_hypothesis when another " +
			"hypothesis of the platform experiment already states the new text. No event announces an edit: an agent reads the " +
			"new text wherever it reads the hypothesis. Returns the hypothesis.",
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Body struct {
			Text string `json:"text"`
		}
	}) (*struct{ Body *domain.Hypothesis }, error) {
		if strings.TrimSpace(in.Body.Text) == "" {
			return nil, huma.Error400BadRequest("text is required")
		}
		hyp, err := loadHypothesis(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		caller, err := changer(ctx, hyp, false)
		if err != nil {
			return nil, err
		}
		if refusal := ChangeRefusal(hyp, caller, false); refusal != nil {
			return nil, refusal
		}
		edited, err := h.svc.EditHypothesis(ctx, in.ID, in.Body.Text)
		if err != nil {
			h.logger.Info("edit hypothesis", zap.Error(err))
			return nil, h.hypothesisWriteError(err)
		}
		return &struct{ Body *domain.Hypothesis }{Body: edited}, nil
	})

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "remove-hypothesis", Method: "DELETE", Path: "/hypotheses/{id}",
		Summary: "Remove an idea you wrote", Tags: []string{"hypotheses"},
		DefaultStatus: 204,
		Description: "Deletes the idea with its outgoing links and its comments, by the person who wrote it or an owner of its org. " +
			changeRule,
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*struct{}, error) {
		hyp, err := loadHypothesis(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		caller, err := changer(ctx, hyp, true)
		if err != nil {
			return nil, err
		}
		if refusal := ChangeRefusal(hyp, caller, true); refusal != nil {
			return nil, refusal
		}
		if err := h.svc.RemoveHypothesis(ctx, in.ID); err != nil {
			h.logger.Info("remove hypothesis", zap.Error(err))
			return nil, h.hypothesisWriteError(err)
		}
		return nil, nil
	})

	apidocs.Register(doc, apidocs.AudienceAgent, apidocs.AfterClose(huma.Operation{
		OperationID: "add-hypothesis-comment", Method: "POST", Path: "/hypotheses/{id}/comments",
		Summary: "Add a comment to a hypothesis or to the objective", Tags: []string{"hypotheses"},
		DefaultStatus: 201,
		Description: "A freeform note with no job behind it (abandon/revise/cross-reference), as opposed to " +
			"a finding, which requires a terminal job. id is a hypothesis, or the platform_experiment_id for the objective, the root " +
			"of its graph: what concerns every approach, such as the published results on the problem. A comment on the objective " +
			"reaches every agent of the platform experiment as comment.new with that id as subject. Read the existing comments " +
			"(GET /hypotheses/{id}/comments) first — don't re-record a conclusion already there. " +
			"Who wrote it is the caller, answered as actor, and no field of the body names it: author is refused. " +
			"agent_id says whose note it is. An agent token's note is its own agent's, named or not. " +
			"Any other caller names an agent it reaches to write that agent's note, or names none and the note is the caller's own. " +
			closedRefusal,
	}), func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Body struct {
			AgentID string `json:"agent_id,omitempty" required:"false" doc:"The agent whose note this is. Absent, it is the caller's own: an agent token's agent, and for any other caller no agent."`
			Text    string `json:"text"`
		}
	}) (*struct {
		Status int
		Body   *domain.HypothesisComment
	}, error) {
		if in.Body.Text == "" {
			return nil, huma.Error400BadRequest("text is required")
		}
		if err := commentsOf(ctx, in.ID); err != nil {
			return nil, err
		}
		owner, actor, err := access.Writer(ctx, in.Body.AgentID)
		if err != nil {
			return nil, err
		}
		c, err := h.svc.AddHypothesisComment(ctx, in.ID, owner, actor, in.Body.Text)
		if err != nil {
			h.logger.Info("add hypothesis comment", zap.Error(err))
			return nil, h.hypothesisWriteError(err)
		}
		return &struct {
			Status int
			Body   *domain.HypothesisComment
		}{Status: http.StatusCreated, Body: c}, nil
	})

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "list-hypothesis-comments", Method: "GET", Path: "/hypotheses/{id}/comments",
		Summary: "List the comments on a hypothesis or on the objective", Tags: []string{"hypotheses"},
		Description: "id is a hypothesis, or the platform_experiment_id for the objective, whose comments no other read " +
			"returns in full. Oldest first, ?limit (default 20, max 200, 422 invalid_page over it) and ?offset; the full count " +
			"is in the X-Total-Count response header.",
	}, func(ctx context.Context, in *struct {
		ID     string `path:"id"`
		Limit  int    `query:"limit" doc:"Page size, default 20, maximum 200. A larger one is refused."`
		Offset int    `query:"offset" doc:"Rows to skip, for paging through X-Total-Count comments."`
	}) (*struct {
		Body       []*domain.HypothesisComment
		TotalCount int `header:"X-Total-Count"`
	}, error) {
		limit, offset, err := page(in.Limit, in.Offset)
		if err != nil {
			return nil, err
		}
		if err := commentsOf(ctx, in.ID); err != nil {
			return nil, err
		}
		comments, total, err := h.svc.ListHypothesisComments(ctx, in.ID, limit, offset)
		if err != nil {
			return nil, auth.Internal(h.logger, "list hypothesis comments", err, zap.String("id", in.ID))
		}
		return &struct {
			Body       []*domain.HypothesisComment
			TotalCount int `header:"X-Total-Count"`
		}{Body: comments, TotalCount: total}, nil
	})

	apidocs.Register(doc, apidocs.AudienceAgent, apidocs.AfterClose(huma.Operation{
		OperationID: "set-hypothesis-status", Method: "POST", Path: "/hypotheses/{id}/status",
		Summary: "Set a hypothesis's status", Tags: []string{"hypotheses"},
		Description: "open (default) / confirmed (validated as a real improvement) / refuted " +
			"(confidently established as not working) / inconclusive (noisy or not worth drilling " +
			"into). Every verdict but open needs a job of the hypothesis that completed or failed, else 409; despite never replaces it. confirmed and refuted are given with the evidence: against names the control, a " +
			"completed job of the same platform experiment, and the hypothesis has two completed " +
			"jobs of its own besides it, or one where every ranking metric of the platform experiment is declared deterministic. Without that the answer is 409 with what is missing, " +
			"unless despite carries a sentence of why the verdict holds anyway: a repeat is needed only where results " +
			"vary between runs. Where every ranking metric is declared deterministic and the hypothesis has a completed job of its own, " +
			"the control is named and despite stands in for nothing (409). A control that holds is the basis even with despite beside it. What a verdict " +
			"rests on is kept as a comment on the hypothesis. " +
			"Only the agent whose hypothesis it is may set it; anyone else gets 403. A " +
			"hypothesis no agent owns may be settled by anyone who reaches it — an idea " +
			"nobody owns would otherwise sit open forever however much evidence it collected. " +
			"agent_id says as which agent the verdict is given: an agent token's is its own, named or not, and any other caller names " +
			"the owning agent to rule for it. Who gave it is the caller, answered as status_actor. " + closedRefusal,
	}), func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Body struct {
			AgentID string                  `json:"agent_id,omitempty" required:"false" doc:"The agent the verdict is given as. Absent, it is the caller's own: an agent token's agent, and for any other caller no agent, which settles only a hypothesis no agent owns."`
			Status  domain.HypothesisStatus `json:"status"`
			Against string                  `json:"against,omitempty" doc:"The control job a confirmed or refuted verdict compares with."`
			Despite string                  `json:"despite,omitempty" doc:"Why a confirmed or refuted verdict holds without a repeated run and a control."`
		}
	}) (*struct{ Body *domain.Hypothesis }, error) {
		if !domain.ValidHypothesisStatus(in.Body.Status) {
			return nil, huma.Error400BadRequest("status must be one of: open, confirmed, refuted, inconclusive")
		}
		if _, err := loadHypothesis(ctx, in.ID); err != nil {
			return nil, err
		}
		owner, actor, err := access.Writer(ctx, in.Body.AgentID)
		if err != nil {
			return nil, err
		}
		hyp, err := h.svc.SetHypothesisStatus(ctx, in.ID, owner, actor, Verdict{Status: in.Body.Status, Against: in.Body.Against, Despite: in.Body.Despite})
		if err != nil {
			h.logger.Info("set hypothesis status", zap.Error(err))
			return nil, h.hypothesisWriteError(err)
		}
		return &struct{ Body *domain.Hypothesis }{Body: hyp}, nil
	})
}
