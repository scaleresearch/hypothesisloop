package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/registry"
)

// closedRefusal is how each hypothesis write states its 409 on a closed platform experiment.
const closedRefusal = "409 experiment_closed once the platform experiment is closed, for every caller. "

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

type hypothesesAPI struct {
	registry *registry.Service
	access   *auth.Access
	logger   *zap.Logger
}

// RegisterRegistry registers the registry's operations: what hangs off an experiment (metrics,
// logs) and the hypothesis notebook. Paths sit under the resource they belong to —
// /experiments/{id}/... and /hypotheses/... — so a caller never has to know which service
// implements them; they are all registered on the one API doc alongside scheduler and quota.
func RegisterRegistry(doc *Doc, svc *registry.Service, logger *zap.Logger) {
	a := hypothesesAPI{registry: svc, access: doc.Access, logger: logger}
	r := jobRecordsAPI{registry: svc, access: doc.Access, logger: logger}
	register(doc, auth.AudienceAgent, huma.Operation{
		OperationID: "check-out-hypothesis", Method: "POST", Path: "/hypotheses/{id}/checkouts",
		Summary: "Record that an agent works on a hypothesis", Tags: []string{"hypotheses"},
		Description: "hl hypothesis checkout calls this once the branch is checked out: from then the agent is in working of " +
			"GET /hypotheses and GET /hypotheses/{id}, its branch in implementations, and the hypothesis in hypotheses_in_flight " +
			"of the situation, for two hours or until a job of its shows it. Checking out again renews it. Nothing is refused for " +
			"another agent on the same hypothesis: the answer names them in working, with a warning. agent_id is the agent's: " +
			"an agent token's own, named or not; any other caller names an agent it reaches. 403 not_signed_up for an agent " +
			"outside the hypothesis's platform experiment.",
	}, a.checkOut)
	// ---- what hangs off an experiment ----

	register(doc, auth.AudienceAgent, huma.Operation{
		OperationID: "list-experiment-data", Method: "GET", Path: "/experiments/{id}/data",
		Summary: "List what a job wrote to durable storage", Tags: []string{"experiments"},
		Description: "Checkpoints and datasets the job left under its own prefix, listed live from the object store. " +
			"A job that kept nothing lists as an empty array — that is the ordinary case, not an error. " +
			"Read any of them from a job, or from your own shell after `eval \"$(hl data-key)\"`: " +
			"HYPOTHESISLOOP_DATA_SHARED spans the whole platform experiment. Each object carries a url a plain GET reads it by for 12 hours, " +
			"with no key: how the data of a job of a prior run is read, since no data key reaches another run.",
	}, r.listData)

	// ---- metrics ----

	register(doc, auth.AudienceAgent, huma.Operation{
		OperationID: "get-metrics", Method: "GET", Path: "/experiments/{id}/metrics",
		Summary: "Get an experiment's metric timeseries", Tags: []string{"metrics"},
		Description: "{metric_name, fraction_complete, metric_value} points.",
	}, r.metrics)

	register(doc, auth.AudienceJob, huma.Operation{
		OperationID: "append-metric", Method: "POST", Path: "/experiments/{id}/metrics",
		Summary: "Append a metric sample for an experiment", Tags: []string{"metrics"},
		DefaultStatus: 204,
		Description: "Records one {metric_name, fraction_complete, metric_value} sample. No server-side validation of the value — never fabricate one. " +
			"A job sends the token it finds in HYPOTHESISLOOP_TOKEN, which is accepted on this call for that job only and until it ends. " +
			"metric_basis defaults to \"raw\": set it only when this value is not on the metric's usual scale (e.g. a different denormalization " +
			"reference than normal) so it is never silently ranked against unmodified runs.",
	}, r.appendMetric)

	// No POST here: log tails are never pushed by the job/research-agent side of the API --
	// only a cluster-agent may report a job's log tail (via /internal/clusters/{name}/status,
	// see clusterexchange), same as job phase. This is read-only.
	register(doc, auth.AudienceAgent, huma.Operation{
		OperationID: "get-log-tail", Method: "GET", Path: "/experiments/{id}/logs",
		Summary: "Get the job's most recently reported log tail", Tags: []string{"metrics"},
		Description: "Returns up to the last `n` lines the owning cluster-agent last reported " +
			"(default 10 -- pass a larger ?n for more, up to however many were last reported). " +
			"Empty (not an error) if none has been reported yet.",
	}, r.logTail)

	register(doc, auth.AudienceAgent, huma.Operation{
		OperationID: "platform-experiment-timeseries", Method: "GET",
		Path:    "/platform-experiments/{id}/metrics-timeseries",
		Summary: "Get per-job metric history across a platform experiment", Tags: []string{"metrics"},
		Description: "One series per job so a dashboard can plot competing agents. Requires ?metric_name; optional ?lookback_hours (default 24) before ?end (default now).",
	}, r.timeseries)

	// ---- hypotheses ----

	// The hypothesis writes are AfterClose so that a coordinator token is refused like every other
	// caller, by the store's 409 experiment_closed, and not first by the gate's 403.
	register(doc, auth.AudienceAgent, afterClose(huma.Operation{
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
	}), a.register)

	register(doc, auth.AudienceAgent, afterClose(huma.Operation{
		OperationID: "add-hypothesis-link", Method: "POST", Path: "/hypotheses/{id}/links",
		Summary: "Link a hypothesis to another", Tags: []string{"hypotheses"},
		DefaultStatus: 201,
		Description: "Adds one outgoing link: builds_on (to a hypothesis, or the platform_experiment_id for the " +
			"objective), contradicts or related (to a hypothesis in the same platform experiment). Only the agent whose " +
			"hypothesis it is may link from it, and one with no owning agent can be linked by anyone; others get 403. " +
			"agent_id says whose link it is, as on registering, and who made it is the caller, answered as actor: author is refused. " +
			"400 on a builds_on cycle, a self link, a target in another platform experiment or an unknown target. Links " +
			"are append-only and re-adding one changes nothing. " + closedRefusal + "Returns the hypothesis's outgoing links.",
	}), a.addLink)

	register(doc, auth.AudienceAgent, read(afterClose(huma.Operation{
		OperationID: "query-hypotheses", Method: "POST", Path: "/hypotheses/query",
		Summary: "Query the hypothesis graph with read-only Cypher", Tags: []string{"hypotheses"},
		Description: registry.QueryDescription,
	})), a.query)

	register(doc, auth.AudienceCoordinator, huma.Operation{
		OperationID: "get-hypothesis-graph", Method: "GET", Path: "/hypotheses/graph",
		Summary: "Get one platform experiment's whole hypothesis graph", Tags: []string{"hypotheses"},
		Description: "Every hypothesis of the platform experiment as a list item (nodes) and every link between them " +
			"(edges, {from, to, type, agent_id, actor, created_at}); an edge to the objective has to = root = the platform_experiment_id. " +
			"A node also carries finding, the newest one filed against it, and best_job {id, metric, summary}, the completed job testing it with the " +
			"best raw value on the first ranking metric and the summary filed for that job; each is absent when there is none. " +
			"objective carries the same two for the root: the jobs of the baseline agent test no hypothesis and hang on the objective. " +
			"It also carries comment_count, the comments on the objective, and comments, the newest five of them, oldest first. Unpaged, " +
			"for drawing the graph.",
	}, a.graph)

	register(doc, auth.AudienceAgent, huma.Operation{
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
	}, a.list)

	register(doc, auth.AudienceCoordinator, huma.Operation{
		OperationID: "hypothesis-stats", Method: "GET", Path: "/hypotheses/stats",
		Summary: "Count hypotheses by status", Tags: []string{"hypotheses"},
		Description: "total and by_status over the hypotheses the ?platform_experiment_id and ?agent_id filters of " +
			"GET /hypotheses match. A status no hypothesis holds is absent.",
	}, a.stats)

	register(doc, auth.AudienceCoordinator, huma.Operation{
		OperationID: "platform-experiment-findings", Method: "GET", Path: "/platform-experiments/{id}/findings",
		Summary: "What a platform experiment has found", Tags: []string{"hypotheses"},
		Description: "Among its 200 newest hypotheses, each one that holds a verdict, a finding or a job with a " +
			"metric, whatever its status, with finding, the newest one filed against it, and best_job {id, metric, summary}, " +
			"the completed job testing it with the best raw value on the first ranking metric and the summary filed for that job, if any. A finding whose created_at is " +
			"later than status_changed_at was filed after the verdict.",
	}, a.findings)

	register(doc, auth.AudienceAgent, huma.Operation{
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
	}, a.get)

	register(doc, auth.AudienceCoordinator, huma.Operation{
		OperationID: "edit-hypothesis", Method: "PATCH", Path: "/hypotheses/{id}",
		Summary: "Edit the text of an idea you wrote", Tags: []string{"hypotheses"},
		Description: "Replaces text, by the person who wrote the idea. " + changeRule + " 409 duplicate_hypothesis when another " +
			"hypothesis of the platform experiment already states the new text. No event announces an edit: an agent reads the " +
			"new text wherever it reads the hypothesis. Returns the hypothesis.",
	}, a.edit)

	register(doc, auth.AudienceCoordinator, huma.Operation{
		OperationID: "remove-hypothesis", Method: "DELETE", Path: "/hypotheses/{id}",
		Summary: "Remove an idea you wrote", Tags: []string{"hypotheses"},
		DefaultStatus: 204,
		Description: "Deletes the idea with its outgoing links and its comments, by the person who wrote it or an owner of its org. " +
			changeRule,
	}, a.remove)

	register(doc, auth.AudienceAgent, afterClose(huma.Operation{
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
	}), a.addComment)

	register(doc, auth.AudienceAgent, huma.Operation{
		OperationID: "list-hypothesis-comments", Method: "GET", Path: "/hypotheses/{id}/comments",
		Summary: "List the comments on a hypothesis or on the objective", Tags: []string{"hypotheses"},
		Description: "id is a hypothesis, or the platform_experiment_id for the objective, whose comments no other read " +
			"returns in full. Oldest first, ?limit (default 20, max 200, 422 invalid_page over it) and ?offset; the full count " +
			"is in the X-Total-Count response header.",
	}, a.listComments)

	register(doc, auth.AudienceAgent, afterClose(huma.Operation{
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
	}), a.setStatus)
}

// loadHypothesis returns a hypothesis the caller reaches. One it does not reach and one that
// does not exist are the same answer.
func (a hypothesesAPI) loadHypothesis(ctx context.Context, id string) (*domain.Hypothesis, error) {
	hyp, err := a.registry.GetHypothesis(ctx, id)
	if err != nil {
		return nil, auth.Internal(a.logger, "get hypothesis", err)
	}
	if hyp == nil {
		return nil, auth.NotFound("hypothesis")
	}
	return hyp, a.access.Within(ctx, "hypothesis", hyp.PlatformExperimentID)
}

// commentsOf admits the caller to the comments of id: a hypothesis it reaches, or the
// objective of a platform experiment it reaches, named by the platform experiment's id.
func (a hypothesesAPI) commentsOf(ctx context.Context, id string) error {
	hyp, err := a.registry.GetHypothesis(ctx, id)
	if err != nil {
		return auth.Internal(a.logger, "get hypothesis", err)
	}
	if hyp != nil {
		return a.access.Within(ctx, "hypothesis", hyp.PlatformExperimentID)
	}
	if err := a.access.Within(ctx, "hypothesis", id); err != nil {
		return err
	}
	if _, err := a.registry.CommentTarget(ctx, id); errors.Is(err, registry.ErrHypothesisNotFound) {
		return auth.NotFound("hypothesis")
	} else if err != nil {
		return auth.Internal(a.logger, "get hypothesis", err)
	}
	return nil
}

// referencesInScope admits the caller to everything a write names as a link target or in
// distinct_from: a hypothesis it reaches, or its platform experiment's id for the objective.
// The service's own refusal for a target elsewhere names that platform experiment.
func (a hypothesesAPI) referencesInScope(ctx context.Context, ids ...string) error {
	for _, id := range ids {
		hyp, err := a.registry.GetHypothesis(ctx, id)
		if err != nil {
			return auth.Internal(a.logger, "get hypothesis", err)
		}
		placedIn := id
		if hyp != nil {
			placedIn = hyp.PlatformExperimentID
		}
		if err := a.access.Within(ctx, "hypothesis", placedIn); err != nil {
			return err
		}
	}
	return nil
}

// changer is who asks to change hyp, as registry.ChangeRefusal reads them. Whether they own its
// org is read only for a removal they did not write.
func (a hypothesesAPI) changer(ctx context.Context, hyp *domain.Hypothesis, remove bool) (registry.Changer, error) {
	p, _ := auth.FromContext(ctx)
	c := registry.Changer{UserID: p.UserID}
	if remove && c.UserID != "" && hyp.Actor.ID != c.UserID {
		var err error
		if c.OrgOwner, err = a.access.OwnsOrgOf(ctx, hyp.PlatformExperimentID); err != nil {
			return registry.Changer{}, auth.Internal(a.logger, "read the caller's org role", err)
		}
	}
	return c, nil
}

const changeRule = "Only for an idea a person wrote for no agent, and only while no agent has acted on it: no job tests it " +
	"and no other hypothesis links to it. Once one has, it is part of the record and the answer is 409 hypothesis_in_record " +
	"naming the jobs and the links; add a comment instead. 403 person_only for a request no signed-in person makes, " +
	"agent_hypothesis for an agent's hypothesis, not_a_persons_idea for one registered with an API key, a coordinator's or " +
	"staff's token, and not_the_author for anyone else. 409 experiment_closed once the platform experiment is closed."

func (a hypothesesAPI) checkOut(ctx context.Context, in *struct {
	ID   string `path:"id"`
	Body struct {
		AgentID string `json:"agent_id,omitempty" required:"false" doc:"The agent that checked it out. Absent, the agent token's own."`
		Branch  string `json:"branch" doc:"The branch checked out for it."`
	}
}) (*struct{ Body *registry.Checkout }, error) {
	if in.Body.Branch == "" {
		return nil, huma.Error400BadRequest("branch is required")
	}
	hyp, err := a.registry.GetHypothesis(ctx, in.ID)
	if err != nil {
		return nil, auth.Internal(a.logger, "get hypothesis", err)
	}
	if hyp == nil {
		return nil, auth.NotFound("hypothesis")
	}
	if err := a.access.Within(ctx, "hypothesis", hyp.PlatformExperimentID); err != nil {
		return nil, err
	}
	agentID, _, err := a.access.Writer(ctx, in.Body.AgentID)
	if err != nil {
		return nil, err
	}
	if agentID == "" {
		return nil, huma.Error400BadRequest("agent_id is required: a checkout is an agent's")
	}
	out, err := a.registry.CheckOut(ctx, hyp, agentID, in.Body.Branch)
	if err != nil {
		return nil, rejected(a.registry.WriteRefusal(err))
	}
	return &struct{ Body *registry.Checkout }{Body: out}, nil
}

func (a hypothesesAPI) register(ctx context.Context, in *struct {
	Body struct {
		AgentID              string            `json:"agent_id,omitempty" required:"false" doc:"The agent whose hypothesis this is. Absent, it is the caller's own: an agent token's agent, and for any other caller no agent."`
		PlatformExperimentID string            `json:"platform_experiment_id"`
		Text                 string            `json:"text"`
		Links                []domain.LinkSpec `json:"links,omitempty" required:"false" doc:"Where the hypothesis sits: at least one {type: builds_on, to: <hypothesis id or the platform_experiment_id for the objective>}; contradicts and related links to hypotheses are optional."`
		DistinctFrom         []string          `json:"distinct_from,omitempty" required:"false" doc:"Ids of hypotheses a 409 listed that you read and judged a different claim from yours."`
	}
}) (*struct {
	Status int
	Body   registry.HypothesisResponse
}, error) {
	b := in.Body
	if b.PlatformExperimentID == "" {
		return nil, huma.Error400BadRequest("platform_experiment_id is required")
	}
	if b.Text == "" {
		return nil, huma.Error400BadRequest("text is required")
	}
	if err := a.access.PlatformExperiment(ctx, b.PlatformExperimentID); err != nil {
		return nil, err
	}
	owner, actor, err := a.access.Writer(ctx, b.AgentID)
	if err != nil {
		return nil, err
	}
	referenced := append([]string{}, b.DistinctFrom...)
	for _, link := range b.Links {
		referenced = append(referenced, link.To)
	}
	if err := a.referencesInScope(ctx, referenced...); err != nil {
		return nil, err
	}
	hyp, links, existed, similar, err := a.registry.RegisterHypothesis(ctx, owner, actor, b.PlatformExperimentID, b.Text, b.Links, b.DistinctFrom)
	if err != nil {
		a.logger.Info("register hypothesis", zap.Error(err))
		return nil, rejected(a.registry.WriteRefusal(err))
	}
	status := http.StatusCreated
	if existed {
		status = http.StatusOK
	}
	return &struct {
		Status int
		Body   registry.HypothesisResponse
	}{Status: status, Body: registry.HypothesisResponse{Hypothesis: hyp, AlreadyExisted: existed, Links: links, Similar: append([]domain.SimilarHypothesis{}, similar...)}}, nil
}

func (a hypothesesAPI) addLink(ctx context.Context, in *struct {
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
	if _, err := a.loadHypothesis(ctx, in.ID); err != nil {
		return nil, err
	}
	if err := a.referencesInScope(ctx, in.Body.To); err != nil {
		return nil, err
	}
	owner, actor, err := a.access.Writer(ctx, in.Body.AgentID)
	if err != nil {
		return nil, err
	}
	links, err := a.registry.AddHypothesisLink(ctx, in.ID, owner, actor, domain.LinkSpec{Type: in.Body.Type, To: in.Body.To})
	if err != nil {
		a.logger.Info("add hypothesis link", zap.Error(err))
		return nil, rejected(a.registry.WriteRefusal(err))
	}
	return &struct {
		Status int
		Body   []domain.Link
	}{Status: http.StatusCreated, Body: links}, nil
}

func (a hypothesesAPI) query(ctx context.Context, in *struct {
	Body struct {
		PlatformExperimentID string         `json:"platform_experiment_id" doc:"The platform experiment whose graph the query reads, bound as $pe."`
		Cypher               string         `json:"cypher"`
		Params               map[string]any `json:"params,omitempty" required:"false" doc:"Query parameters, referenced as $name; pe and evidence are bound by the platform and refused here."`
	}
}) (*struct{ Body *registry.QueryResult }, error) {
	if in.Body.PlatformExperimentID == "" || in.Body.Cypher == "" {
		return nil, huma.Error400BadRequest("platform_experiment_id and cypher are required")
	}
	if err := a.access.PlatformExperiment(ctx, in.Body.PlatformExperimentID); err != nil {
		return nil, err
	}
	result, err := a.registry.QueryGraph(ctx, in.Body.PlatformExperimentID, in.Body.Cypher, in.Body.Params)
	if err != nil {
		if errors.Is(err, registry.ErrInvalidQuery) {
			return nil, huma.Error400BadRequest(err.Error())
		}
		return nil, auth.Internal(a.logger, "query hypotheses", err)
	}
	return &struct{ Body *registry.QueryResult }{Body: result}, nil
}

func (a hypothesesAPI) graph(ctx context.Context, in *struct {
	PlatformExperimentID string `query:"platform_experiment_id" doc:"Required."`
}) (*struct{ Body *registry.HypothesisGraph }, error) {
	if in.PlatformExperimentID == "" {
		return nil, huma.Error400BadRequest("platform_experiment_id is required")
	}
	if err := a.access.PlatformExperiment(ctx, in.PlatformExperimentID); err != nil {
		return nil, err
	}
	graph, err := a.registry.HypothesisGraph(ctx, in.PlatformExperimentID)
	if errors.Is(err, registry.ErrUnknownPlatformExperiment) {
		return nil, huma.Error404NotFound(err.Error())
	}
	if err != nil {
		return nil, auth.Internal(a.logger, "get hypothesis graph", err)
	}
	return &struct{ Body *registry.HypothesisGraph }{Body: graph}, nil
}

func (a hypothesesAPI) list(ctx context.Context, in *struct {
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
	Body       []*registry.GraphNode
	TotalCount int `header:"X-Total-Count"`
}, error) {
	if in.Status != "" && !domain.ValidHypothesisStatus(in.Status) {
		return nil, huma.Error400BadRequest(fmt.Sprintf("invalid status %q", in.Status))
	}
	limit, offset, err := page(in.Limit, in.Offset)
	if err != nil {
		return nil, err
	}
	within, restricted, err := a.access.PlatformExperimentIDs(ctx, in.ProjectID)
	if err != nil {
		return nil, err
	}
	filter := registry.HypothesisFilter{PlatformExperimentID: in.PlatformExperimentID, AgentID: in.Agent,
		Restricted: restricted, Within: within, Status: in.Status, ByPeople: in.By == "people", Search: in.Search, Limit: limit, Offset: offset}
	if in.Untaken {
		if filter.ExcludeIDs, err = a.registry.TakenHypotheses(ctx, in.PlatformExperimentID); err != nil {
			return nil, auth.Internal(a.logger, "list hypotheses", err)
		}
	}
	hs, err := a.registry.ListHypotheses(ctx, filter)
	if err != nil {
		return nil, auth.Internal(a.logger, "list hypotheses", err)
	}
	stats, err := a.registry.HypothesisStats(ctx, filter)
	if err != nil {
		return nil, auth.Internal(a.logger, "list hypotheses", err)
	}
	return &struct {
		Body       []*registry.GraphNode
		TotalCount int `header:"X-Total-Count"`
	}{Body: hs, TotalCount: stats.Total}, nil
}

func (a hypothesesAPI) stats(ctx context.Context, in *struct {
	PlatformExperimentID string `query:"platform_experiment_id" doc:"Restrict to one platform experiment."`
	ProjectID            string `query:"project_id" doc:"Only hypotheses of this project's platform experiments."`
	Agent                string `query:"agent_id" doc:"Only hypotheses registered by this agent."`
}) (*struct{ Body *domain.HypothesisStats }, error) {
	within, restricted, err := a.access.PlatformExperimentIDs(ctx, in.ProjectID)
	if err != nil {
		return nil, err
	}
	stats, err := a.registry.HypothesisStats(ctx, registry.HypothesisFilter{PlatformExperimentID: in.PlatformExperimentID, AgentID: in.Agent,
		Restricted: restricted, Within: within})
	if err != nil {
		return nil, auth.Internal(a.logger, "hypothesis stats", err)
	}
	return &struct{ Body *domain.HypothesisStats }{Body: stats}, nil
}

func (a hypothesesAPI) findings(ctx context.Context, in *struct {
	ID string `path:"id"`
}) (*struct {
	Body []*registry.HypothesisEvidence
}, error) {
	if err := a.access.PlatformExperiment(ctx, in.ID); err != nil {
		return nil, err
	}
	findings, err := a.registry.Findings(ctx, in.ID)
	if errors.Is(err, registry.ErrUnknownPlatformExperiment) {
		return nil, huma.Error404NotFound(err.Error())
	}
	if err != nil {
		return nil, auth.Internal(a.logger, "platform experiment findings", err, zap.String("id", in.ID))
	}
	return &struct {
		Body []*registry.HypothesisEvidence
	}{Body: findings}, nil
}

func (a hypothesesAPI) get(ctx context.Context, in *struct {
	ID     string `path:"id"`
	Limit  int    `query:"limit" doc:"Page size applied to each of jobs, findings and comments alike; default 20, maximum 200. A larger one is refused. job_count/finding_count/comment_count report the full size of each set."`
	Offset int    `query:"offset" doc:"Rows to skip within each of the three lists."`
}) (*struct{ Body hypothesisWithJobs }, error) {
	var err error
	if in.Limit, in.Offset, err = page(in.Limit, in.Offset); err != nil {
		return nil, err
	}
	hyp, err := a.loadHypothesis(ctx, in.ID)
	if err != nil {
		if a.commentsOf(ctx, in.ID) == nil {
			return nil, huma.Error404NotFound(fmt.Sprintf("%s is the objective, no hypothesis: GET /hypotheses/%s/comments lists the comments on it, and its jobs are the baseline's", in.ID, in.ID))
		}
		return nil, err
	}
	if err := a.registry.NameHypothesis(ctx, hyp); err != nil {
		return nil, auth.Internal(a.logger, "name the hypothesis's actors", err)
	}
	links, err := a.registry.GetHypothesisLinks(ctx, in.ID)
	if err != nil {
		return nil, auth.Internal(a.logger, "get hypothesis", err, zap.String("id", in.ID))
	}
	implementations, err := a.registry.Implementations(ctx, in.ID)
	if err != nil {
		return nil, auth.Internal(a.logger, "get hypothesis", err, zap.String("id", in.ID))
	}
	working, err := a.registry.Working(ctx, []string{in.ID})
	if err != nil {
		return nil, auth.Internal(a.logger, "get hypothesis", err, zap.String("id", in.ID))
	}
	baseRef, err := a.registry.BaseRef(ctx, hyp)
	if err != nil {
		return nil, auth.Internal(a.logger, "get hypothesis", err, zap.String("id", in.ID))
	}
	jobFilter := domain.ExperimentFilter{Scope: domain.AllTenants, HypothesisID: in.ID}
	jobs, err := a.registry.List(ctx, domain.ExperimentFilter{
		Scope: domain.AllTenants, HypothesisID: in.ID, Limit: in.Limit, Offset: in.Offset,
	})
	if err != nil {
		return nil, auth.Internal(a.logger, "get hypothesis", err, zap.String("id", in.ID))
	}
	jobCount, err := a.registry.Count(ctx, jobFilter)
	if err != nil {
		return nil, auth.Internal(a.logger, "get hypothesis", err, zap.String("id", in.ID))
	}
	findings, findingCount, err := a.registry.ListHypothesisFindings(ctx, in.ID, in.Limit, in.Offset)
	if err != nil {
		return nil, auth.Internal(a.logger, "get hypothesis", err, zap.String("id", in.ID))
	}
	comments, commentCount, err := a.registry.ListHypothesisComments(ctx, in.ID, in.Limit, in.Offset)
	if err != nil {
		return nil, auth.Internal(a.logger, "get hypothesis", err, zap.String("id", in.ID))
	}
	return &struct{ Body hypothesisWithJobs }{Body: hypothesisWithJobs{
		Hypothesis: hyp,
		Links:      links, Implementations: implementations, Working: append([]domain.HypothesisWorker{}, working[in.ID]...), BaseRef: baseRef,
		Jobs: jobs, JobCount: jobCount,
		Findings: findings, FindingCount: findingCount,
		Comments: comments, CommentCount: commentCount,
	}}, nil
}

func (a hypothesesAPI) edit(ctx context.Context, in *struct {
	ID   string `path:"id"`
	Body struct {
		Text string `json:"text"`
	}
}) (*struct{ Body *domain.Hypothesis }, error) {
	if strings.TrimSpace(in.Body.Text) == "" {
		return nil, huma.Error400BadRequest("text is required")
	}
	hyp, err := a.loadHypothesis(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	caller, err := a.changer(ctx, hyp, false)
	if err != nil {
		return nil, err
	}
	if refusal := registry.ChangeRefusal(hyp, caller, false); refusal != nil {
		return nil, refusal
	}
	edited, err := a.registry.EditHypothesis(ctx, in.ID, in.Body.Text)
	if err != nil {
		a.logger.Info("edit hypothesis", zap.Error(err))
		return nil, rejected(a.registry.WriteRefusal(err))
	}
	return &struct{ Body *domain.Hypothesis }{Body: edited}, nil
}

func (a hypothesesAPI) remove(ctx context.Context, in *struct {
	ID string `path:"id"`
}) (*struct{}, error) {
	hyp, err := a.loadHypothesis(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	caller, err := a.changer(ctx, hyp, true)
	if err != nil {
		return nil, err
	}
	if refusal := registry.ChangeRefusal(hyp, caller, true); refusal != nil {
		return nil, refusal
	}
	if err := a.registry.RemoveHypothesis(ctx, in.ID); err != nil {
		a.logger.Info("remove hypothesis", zap.Error(err))
		return nil, rejected(a.registry.WriteRefusal(err))
	}
	return nil, nil
}

func (a hypothesesAPI) addComment(ctx context.Context, in *struct {
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
	if err := a.commentsOf(ctx, in.ID); err != nil {
		return nil, err
	}
	owner, actor, err := a.access.Writer(ctx, in.Body.AgentID)
	if err != nil {
		return nil, err
	}
	c, err := a.registry.AddHypothesisComment(ctx, in.ID, owner, actor, in.Body.Text)
	if err != nil {
		a.logger.Info("add hypothesis comment", zap.Error(err))
		return nil, rejected(a.registry.WriteRefusal(err))
	}
	return &struct {
		Status int
		Body   *domain.HypothesisComment
	}{Status: http.StatusCreated, Body: c}, nil
}

func (a hypothesesAPI) listComments(ctx context.Context, in *struct {
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
	if err := a.commentsOf(ctx, in.ID); err != nil {
		return nil, err
	}
	comments, total, err := a.registry.ListHypothesisComments(ctx, in.ID, limit, offset)
	if err != nil {
		return nil, auth.Internal(a.logger, "list hypothesis comments", err, zap.String("id", in.ID))
	}
	return &struct {
		Body       []*domain.HypothesisComment
		TotalCount int `header:"X-Total-Count"`
	}{Body: comments, TotalCount: total}, nil
}

func (a hypothesesAPI) setStatus(ctx context.Context, in *struct {
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
	if _, err := a.loadHypothesis(ctx, in.ID); err != nil {
		return nil, err
	}
	owner, actor, err := a.access.Writer(ctx, in.Body.AgentID)
	if err != nil {
		return nil, err
	}
	hyp, err := a.registry.SetHypothesisStatus(ctx, in.ID, owner, actor, registry.Verdict{Status: in.Body.Status, Against: in.Body.Against, Despite: in.Body.Despite})
	if err != nil {
		a.logger.Info("set hypothesis status", zap.Error(err))
		return nil, rejected(a.registry.WriteRefusal(err))
	}
	return &struct{ Body *domain.Hypothesis }{Body: hyp}, nil
}
