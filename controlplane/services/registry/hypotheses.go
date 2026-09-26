package registry

import (
	"context"
	"errors"
	"fmt"
	"maps"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/graphdb"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
)

var (
	// ErrHypothesisNotFound and ErrHypothesisNotOwner let the HTTP layer answer 404 and 403.
	ErrHypothesisNotFound = graphdb.ErrNotFound
	ErrHypothesisNotOwner = graphdb.ErrNotOwner
	// ErrHypothesisSubmitterNotAllowed is returned by RegisterHypothesis when the platform
	// experiment's HypothesisSubmitPolicy forbids the submission's source (human or agent).
	ErrHypothesisSubmitterNotAllowed = errors.New("hypothesis submitter not allowed by platform experiment's hypothesis_submit_policy")
	// ErrUnknownPlatformExperiment is a registration naming a platform experiment that does not exist.
	ErrUnknownPlatformExperiment = errors.New("unknown platform_experiment_id")
)

// RegisterHypothesis registers a new hypothesis within a platform experiment, placed in the graph
// by links (at least one builds_on), or returns the existing one (alreadyExisted=true, links not
// applied) if an equivalent one by normalized text is already registered in that platform
// experiment. The returned links are the hypothesis's outgoing links. While it reads close to
// hypotheses not named in distinctFrom or a contradicts/related link, nothing is created and the
// error wraps a *graphdb.SimilarError listing them.
//
// agentID is the agent whose hypothesis it is, empty for one no agent owns, which the submit
// policy reads as a human's. actor is the credential registering it.
func (s *Service) RegisterHypothesis(ctx context.Context, agentID string, actor domain.Actor, platformExperimentID, text string, links []domain.LinkSpec, distinctFrom []string) (h *domain.Hypothesis, out []domain.Link, alreadyExisted bool, err error) {
	hypothesisPolicy, _, found, err := s.store.GetPlatformExperimentSubmitPolicies(ctx, platformExperimentID)
	if err != nil {
		return nil, nil, false, fmt.Errorf("registry.RegisterHypothesis: %w", err)
	}
	if !found {
		return nil, nil, false, fmt.Errorf("%w: %s", ErrUnknownPlatformExperiment, platformExperimentID)
	}
	if agentID == "" {
		if !hypothesisPolicy.AllowsHuman() {
			return nil, nil, false, fmt.Errorf("%w: platform experiment %s accepts hypotheses from agents only — this one names no agent_id, which makes it a human submission; read the experiment's hypothesis_submit_policy before retrying, a human identity cannot submit hypotheses here",
				ErrHypothesisSubmitterNotAllowed, platformExperimentID)
		}
	} else {
		if !hypothesisPolicy.AllowsAgent() {
			return nil, nil, false, fmt.Errorf("%w: platform experiment %s accepts hypotheses from humans only — this is an autonomous-agent submission (agent_id=%s); read the experiment's hypothesis_submit_policy before retrying, an agent identity cannot submit hypotheses here",
				ErrHypothesisSubmitterNotAllowed, platformExperimentID, agentID)
		}
		if err := s.requireAgent(ctx, agentID); err != nil {
			return nil, nil, false, fmt.Errorf("registry.RegisterHypothesis: %w", err)
		}
	}

	err = s.events.Emit(ctx, func(stamp db.Stamp) ([]db.Event, error) {
		var announce bool
		h, out, alreadyExisted, announce, err = s.graph.Register(ctx, graphdb.NewHypothesis{
			AgentID: agentID, Actor: actor,
			PlatformExperimentID: platformExperimentID, Text: text, Links: links, DistinctFrom: distinctFrom,
			Stamp: stamp,
		})
		if err != nil || !announce {
			return nil, err
		}
		return []db.Event{{Kind: db.EventHypothesisNew, Subject: h.ID,
			PlatformExperimentID: h.PlatformExperimentID, AgentID: h.AgentID}}, nil
	})
	var similar *graphdb.SimilarError
	if errors.As(err, &similar) {
		named := make([]*domain.Actor, len(similar.Similar))
		for i := range similar.Similar {
			named[i] = &similar.Similar[i].Actor
		}
		if nameErr := s.store.NameActors(ctx, named); nameErr != nil {
			return nil, nil, false, fmt.Errorf("registry.RegisterHypothesis: %w", nameErr)
		}
	}
	if err != nil {
		return nil, nil, false, fmt.Errorf("registry.RegisterHypothesis: %w", err)
	}
	if err := s.store.NameActors(ctx, append(linkActors(out), hypothesisActors(h)...)); err != nil {
		return nil, nil, false, fmt.Errorf("registry.RegisterHypothesis: %w", err)
	}
	s.logger.Info("hypothesis registered",
		zap.String("id", h.ID), zap.String("agent", agentID), zap.String("actor_kind", string(actor.Kind)), zap.String("actor_id", actor.ID),
		zap.String("platform_experiment_id", platformExperimentID), zap.Bool("already_existed", alreadyExisted))
	return h, out, alreadyExisted, nil
}

// hypothesisActors and linkActors are the actors a hypothesis and its links carry, for naming.
func hypothesisActors(h *domain.Hypothesis) []*domain.Actor {
	if h.StatusActor == nil {
		return []*domain.Actor{&h.Actor}
	}
	return []*domain.Actor{&h.Actor, h.StatusActor}
}

func linkActors(links []domain.Link) []*domain.Actor {
	actors := make([]*domain.Actor, len(links))
	for i := range links {
		actors[i] = &links[i].Actor
	}
	return actors
}

func itemActors(items []*domain.HypothesisListItem) []*domain.Actor {
	var actors []*domain.Actor
	for _, item := range items {
		actors = append(actors, hypothesisActors(item.Hypothesis)...)
	}
	return actors
}

// AddHypothesisLink adds one outgoing link from an existing hypothesis on behalf of its registering
// agent, or of anyone when no agent owns it. Re-adding an existing link changes nothing. Returns
// the hypothesis's outgoing links. agentID is the agent whose link it is, empty when no agent
// owns it, and actor the credential adding it.
func (s *Service) AddHypothesisLink(ctx context.Context, id, agentID string, actor domain.Actor, spec domain.LinkSpec) ([]domain.Link, error) {
	if agentID != "" {
		if err := s.requireAgent(ctx, agentID); err != nil {
			return nil, fmt.Errorf("registry.AddHypothesisLink: %w", err)
		}
	}
	var result *graphdb.LinkResult
	err := s.events.Emit(ctx, func(stamp db.Stamp) ([]db.Event, error) {
		var err error
		result, err = s.graph.AddLink(ctx, id, agentID, actor, spec, stamp)
		if err != nil || !result.Announce {
			return nil, err
		}
		return []db.Event{{Kind: db.EventHypothesisLink, Subject: id, Value: string(spec.Type), Detail: spec.To,
			PlatformExperimentID: result.From.PlatformExperimentID, AgentID: agentID}}, nil
	})
	if err != nil {
		return nil, fmt.Errorf("registry.AddHypothesisLink: %w", err)
	}
	s.logger.Info("hypothesis link added",
		zap.String("from", id), zap.String("type", string(spec.Type)), zap.String("to", spec.To),
		zap.String("agent", agentID), zap.String("actor_kind", string(actor.Kind)), zap.String("actor_id", actor.ID), zap.Bool("created", result.Created))
	if err := s.store.NameActors(ctx, linkActors(result.Out)); err != nil {
		return nil, fmt.Errorf("registry.AddHypothesisLink: %w", err)
	}
	return result.Out, nil
}

// GetHypothesis returns a single hypothesis by ID, nil when there is none. Its actors carry no
// name until NameHypothesis gives them one: most callers read it to decide and answer nothing.
func (s *Service) GetHypothesis(ctx context.Context, id string) (*domain.Hypothesis, error) {
	h, err := s.graph.GetHypothesis(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("registry.GetHypothesis: %w", err)
	}
	return h, nil
}

// NameHypothesis names the actors of a hypothesis that is about to be answered.
func (s *Service) NameHypothesis(ctx context.Context, h *domain.Hypothesis) error {
	if err := s.store.NameActors(ctx, hypothesisActors(h)); err != nil {
		return fmt.Errorf("registry.NameHypothesis: %w", err)
	}
	return nil
}

// GetHypothesisLinks returns the links touching one hypothesis, both directions.
func (s *Service) GetHypothesisLinks(ctx context.Context, id string) (domain.HypothesisLinks, error) {
	links, err := s.graph.Links(ctx, id)
	if err != nil {
		return domain.HypothesisLinks{}, fmt.Errorf("registry.GetHypothesisLinks: %w", err)
	}
	if err := s.store.NameActors(ctx, append(linkActors(links.Out), linkActors(links.In)...)); err != nil {
		return domain.HypothesisLinks{}, fmt.Errorf("registry.GetHypothesisLinks: %w", err)
	}
	return links, nil
}

// ListHypotheses returns one page of hypotheses, most recent first, each carrying its place in the
// graph and its finding/comment counts. Bounded and defaulted by the graph store.
func (s *Service) ListHypotheses(ctx context.Context, f graphdb.ListFilter) ([]*domain.HypothesisListItem, error) {
	if f.Status != "" && !domain.ValidHypothesisStatus(f.Status) {
		return nil, fmt.Errorf("registry.ListHypotheses: invalid status %q", f.Status)
	}
	items, err := s.graph.ListHypotheses(ctx, f)
	if err != nil {
		return nil, fmt.Errorf("registry.ListHypotheses: %w", err)
	}
	if err := s.addActivity(ctx, items); err != nil {
		return nil, fmt.Errorf("registry.ListHypotheses: %w", err)
	}
	if err := s.store.NameActors(ctx, itemActors(items)); err != nil {
		return nil, fmt.Errorf("registry.ListHypotheses: %w", err)
	}
	return items, nil
}

// HypothesisStats counts the hypotheses matching the same filters by status, ignoring limit/offset.
func (s *Service) HypothesisStats(ctx context.Context, f graphdb.ListFilter) (*domain.HypothesisStats, error) {
	stats, err := s.graph.HypothesisStats(ctx, f)
	if err != nil {
		return nil, fmt.Errorf("registry.HypothesisStats: %w", err)
	}
	return stats, nil
}

// GraphNode is a hypothesis in its platform experiment's graph with what was found for it.
type GraphNode struct {
	*domain.HypothesisListItem
	Evidence
}

// HypothesisGraph returns the whole hypothesis graph of one platform experiment, each node with
// its newest finding and its best job.
func (s *Service) HypothesisGraph(ctx context.Context, platformExperimentID string) ([]*GraphNode, []domain.GraphEdge, error) {
	metrics, found, err := s.store.GetPlatformExperimentMetrics(ctx, platformExperimentID)
	if err != nil {
		return nil, nil, fmt.Errorf("registry.HypothesisGraph: %w", err)
	}
	if !found {
		return nil, nil, fmt.Errorf("%w: %s", ErrUnknownPlatformExperiment, platformExperimentID)
	}
	items, edges, err := s.graph.ExperimentGraph(ctx, platformExperimentID)
	if err != nil {
		return nil, nil, fmt.Errorf("registry.HypothesisGraph: %w", err)
	}
	if err := s.addActivity(ctx, items); err != nil {
		return nil, nil, fmt.Errorf("registry.HypothesisGraph: %w", err)
	}
	evidence, err := s.evidence(ctx, metrics, items)
	if err != nil {
		return nil, nil, fmt.Errorf("registry.HypothesisGraph: %w", err)
	}
	nodes := make([]*GraphNode, len(items))
	actors := itemActors(items)
	for i, item := range items {
		nodes[i] = &GraphNode{HypothesisListItem: item, Evidence: evidence[item.ID]}
		if f := nodes[i].Finding; f != nil {
			actors = append(actors, &f.Actor)
		}
	}
	for i := range edges {
		actors = append(actors, &edges[i].Actor)
	}
	if err := s.store.NameActors(ctx, actors); err != nil {
		return nil, nil, fmt.Errorf("registry.HypothesisGraph: %w", err)
	}
	return nodes, edges, nil
}

// QueryGraph runs one read-only Cypher query against one platform experiment's hypothesis graph.
func (s *Service) QueryGraph(ctx context.Context, platformExperimentID, cypher string, params map[string]any) (*graphdb.QueryResult, error) {
	if _, ok := params["evidence"]; ok {
		return nil, fmt.Errorf("%w: $evidence is already bound to what Postgres holds per hypothesis: use $evidence in the query without passing it, and name your own parameter differently", graphdb.ErrInvalidQuery)
	}
	evidence, err := s.store.Evidence(ctx, platformExperimentID)
	if err != nil {
		return nil, fmt.Errorf("registry.QueryGraph: %w", err)
	}
	bound := maps.Clone(params)
	if bound == nil {
		bound = map[string]any{}
	}
	bound["evidence"] = evidence
	result, err := s.graph.Query(ctx, platformExperimentID, cypher, bound)
	if err != nil {
		return nil, fmt.Errorf("registry.QueryGraph: %w", err)
	}
	return result, nil
}

// Evidence is what was found for one hypothesis.
type Evidence struct {
	// Finding is the newest finding filed against it, nil when none was.
	Finding *domain.HypothesisFinding `json:"finding,omitempty"`
	// BestJob is the job testing it that holds the best raw value on the platform experiment's
	// first ranking metric, nil when none reported it.
	BestJob *BestJob `json:"best_job,omitempty"`
}

// HypothesisEvidence is a hypothesis with what was found for it.
type HypothesisEvidence struct {
	*domain.Hypothesis
	Evidence
}

type BestJob struct {
	ID     string            `json:"id"`
	Metric *domain.JobMetric `json:"metric"`
	// Summary is the write-up filed for this job, absent when none was.
	Summary string `json:"summary,omitempty"`
}

// maxFindings bounds how many hypotheses Findings reads.
const maxFindings = 200

// Findings returns what a platform experiment has found: among its newest maxFindings hypotheses,
// each one that holds a verdict, a finding or a job with a metric, with its newest finding and its
// best job, which carries its own summary.
func (s *Service) Findings(ctx context.Context, platformExperimentID string) ([]*HypothesisEvidence, error) {
	metrics, found, err := s.store.GetPlatformExperimentMetrics(ctx, platformExperimentID)
	if err != nil {
		return nil, fmt.Errorf("registry.Findings: %w", err)
	}
	if !found {
		return nil, fmt.Errorf("%w: %s", ErrUnknownPlatformExperiment, platformExperimentID)
	}
	hypotheses, err := s.graph.ListHypotheses(ctx, graphdb.ListFilter{PlatformExperimentID: platformExperimentID, Limit: maxFindings})
	if err != nil {
		return nil, fmt.Errorf("registry.Findings: %w", err)
	}
	evidence, err := s.evidence(ctx, metrics, hypotheses)
	if err != nil {
		return nil, fmt.Errorf("registry.Findings: %w", err)
	}
	out := []*HypothesisEvidence{}
	var actors []*domain.Actor
	for _, h := range hypotheses {
		e := &HypothesisEvidence{Hypothesis: h.Hypothesis, Evidence: evidence[h.ID]}
		if h.Status == domain.HypothesisOpen && e.Finding == nil && e.BestJob == nil {
			continue
		}
		out = append(out, e)
		actors = append(actors, hypothesisActors(e.Hypothesis)...)
		if e.Finding != nil {
			actors = append(actors, &e.Finding.Actor)
		}
	}
	if err := s.store.NameActors(ctx, actors); err != nil {
		return nil, fmt.Errorf("registry.Findings: %w", err)
	}
	return out, nil
}

// evidence returns, by hypothesis id, the newest finding of each of the hypotheses and the job
// testing it that did best on the first ranking metric of metrics, with that job's own summary.
// It is a fixed number of reads however many hypotheses are given.
func (s *Service) evidence(ctx context.Context, metrics []domain.MetricDefinition, hypotheses []*domain.HypothesisListItem) (map[string]Evidence, error) {
	ids := make([]string, len(hypotheses))
	for i, h := range hypotheses {
		ids[i] = h.ID
	}
	newest, err := s.store.NewestFindings(ctx, ids)
	if err != nil {
		return nil, err
	}
	jobs, err := s.store.ListExperimentsTesting(ctx, ids)
	if err != nil {
		return nil, err
	}
	if err := metricsdb.PopulateJobMetrics(ctx, s.metricsDBURL, s.store.GetPlatformExperimentMetrics, jobs); err != nil {
		return nil, err
	}
	primary, _ := domain.PrimaryRankingMetric(metrics)
	best := map[string]*domain.Experiment{}
	for _, job := range jobs {
		if job.Metric == nil || job.Metric.Basis != "raw" {
			continue
		}
		if cur := best[job.HypothesisID]; cur != nil {
			better := job.Metric.Best > cur.Metric.Best
			if primary.Direction == "minimize" {
				better = job.Metric.Best < cur.Metric.Best
			}
			if !better {
				continue
			}
		}
		best[job.HypothesisID] = job
	}
	bestIDs := make([]string, 0, len(best))
	for _, job := range best {
		bestIDs = append(bestIDs, job.ID)
	}
	summaries, err := s.store.FindingSummaries(ctx, bestIDs)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Evidence, len(ids))
	for _, id := range ids {
		e := Evidence{Finding: newest[id]}
		if job := best[id]; job != nil {
			e.BestJob = &BestJob{ID: job.ID, Metric: job.Metric, Summary: summaries[job.ID]}
		}
		out[id] = e
	}
	return out, nil
}

// addActivity fills the finding/comment counts of items from Postgres, in one query.
func (s *Service) addActivity(ctx context.Context, items []*domain.HypothesisListItem) error {
	ids := make([]string, len(items))
	for i, item := range items {
		ids[i] = item.ID
	}
	activity, err := s.store.CountHypothesisActivity(ctx, ids)
	if err != nil {
		return err
	}
	for _, item := range items {
		item.FindingCount = activity[item.ID].Findings
		item.CommentCount = activity[item.ID].Comments
	}
	return nil
}

// SetHypothesisStatus sets a hypothesis's status on behalf of callerAgentID: the owning agent's
// own verdict on its own claim, or anyone's on a hypothesis no agent owns, for which
// callerAgentID may be empty (see domain.HypothesisStatus). Ownership is checked in the same
// graph transaction as the write. actor is the credential giving the verdict, kept on the
// hypothesis as its status_actor.
//
// A confirmed or refuted verdict is given only with the evidence for it (see supported), and
// what it rests on is kept as a comment on the hypothesis, the agent's when one is named and
// written by actor.
func (s *Service) SetHypothesisStatus(ctx context.Context, id, callerAgentID string, actor domain.Actor, verdict Verdict) (*domain.Hypothesis, error) {
	status := verdict.Status
	if !domain.ValidHypothesisStatus(status) {
		return nil, fmt.Errorf("registry.SetHypothesisStatus: invalid status %q", status)
	}
	current, err := s.graph.GetHypothesis(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("registry.SetHypothesisStatus: %w", err)
	}
	// Whose verdict it is comes first and is the graph's to refuse, in the write below: the
	// evidence is asked only of an agent that may give the verdict at all.
	mayJudge := current != nil && (current.AgentID == "" || current.AgentID == callerAgentID)
	if mayJudge && current.Status != status {
		if err := s.supported(ctx, current, verdict); err != nil {
			return nil, fmt.Errorf("registry.SetHypothesisStatus: %w", err)
		}
	}
	var h *domain.Hypothesis
	var announced bool
	err = s.events.Emit(ctx, func(stamp db.Stamp) ([]db.Event, error) {
		var err error
		h, announced, err = s.graph.UpdateStatus(ctx, id, callerAgentID, actor, status, stamp)
		if err != nil || !announced {
			return nil, err
		}
		return []db.Event{{Kind: db.EventHypothesisStatus, Subject: h.ID, Value: string(h.Status),
			PlatformExperimentID: h.PlatformExperimentID, AgentID: h.AgentID}}, nil
	})
	if err != nil {
		return nil, fmt.Errorf("registry.SetHypothesisStatus: %w", err)
	}
	s.logger.Info("hypothesis status updated",
		zap.String("id", id), zap.String("agent", callerAgentID), zap.String("status", string(status)), zap.Bool("announced", announced))
	if announced && (status == domain.HypothesisConfirmed || status == domain.HypothesisRefuted) {
		if _, err := s.AddHypothesisComment(ctx, h.ID, callerAgentID, actor, basis(verdict)); err != nil {
			return nil, fmt.Errorf("registry.SetHypothesisStatus: the verdict is set and its basis could not be noted: %w", err)
		}
	}
	if err := s.store.NameActors(ctx, hypothesisActors(h)); err != nil {
		return nil, fmt.Errorf("registry.SetHypothesisStatus: %w", err)
	}
	return h, nil
}

// ListHypothesisFindings returns one page of the findings filed against a hypothesis, oldest
// first, alongside the full count the page was taken from.
func (s *Service) ListHypothesisFindings(ctx context.Context, hypothesisID string, limit, offset int) ([]*domain.HypothesisFinding, int, error) {
	fs, err := s.store.ListFindingsByHypothesis(ctx, hypothesisID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("registry.ListHypothesisFindings: %w", err)
	}
	total, err := s.store.CountFindingsByHypothesis(ctx, hypothesisID)
	if err != nil {
		return nil, 0, fmt.Errorf("registry.ListHypothesisFindings: %w", err)
	}
	actors := make([]*domain.Actor, len(fs))
	for i, f := range fs {
		actors[i] = &f.Actor
	}
	if err := s.store.NameActors(ctx, actors); err != nil {
		return nil, 0, fmt.Errorf("registry.ListHypothesisFindings: %w", err)
	}
	return fs, total, nil
}

// AddHypothesisComment records a freeform, job-independent note against a hypothesis — see
// domain.HypothesisComment for how this differs from a finding. agentID is the agent whose note
// it is, empty for one no agent owns, and actor the credential writing it.
func (s *Service) AddHypothesisComment(ctx context.Context, hypothesisID, agentID string, actor domain.Actor, text string) (*domain.HypothesisComment, error) {
	h, err := s.graph.GetHypothesis(ctx, hypothesisID)
	if err != nil {
		return nil, fmt.Errorf("registry.AddHypothesisComment: %w", err)
	}
	if h == nil {
		return nil, fmt.Errorf("registry.AddHypothesisComment: %w: %s", ErrHypothesisNotFound, hypothesisID)
	}
	c, err := s.store.CreateHypothesisComment(ctx, hypothesisID, h.PlatformExperimentID, agentID, actor, text)
	if err != nil {
		return nil, fmt.Errorf("registry.AddHypothesisComment: %w", err)
	}
	if err := s.store.NameActors(ctx, []*domain.Actor{&c.Actor}); err != nil {
		return nil, fmt.Errorf("registry.AddHypothesisComment: %w", err)
	}
	s.logger.Info("hypothesis comment added",
		zap.String("hypothesis_id", hypothesisID), zap.String("agent", agentID),
		zap.String("actor_kind", string(actor.Kind)), zap.String("actor_id", actor.ID))
	return c, nil
}

// ListHypothesisComments returns one page of the comments filed against a hypothesis, oldest
// first, alongside the full count the page was taken from.
func (s *Service) ListHypothesisComments(ctx context.Context, hypothesisID string, limit, offset int) ([]*domain.HypothesisComment, int, error) {
	cs, err := s.store.ListCommentsByHypothesis(ctx, hypothesisID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("registry.ListHypothesisComments: %w", err)
	}
	actors := make([]*domain.Actor, len(cs))
	for i, c := range cs {
		actors[i] = &c.Actor
	}
	if err := s.store.NameActors(ctx, actors); err != nil {
		return nil, 0, fmt.Errorf("registry.ListHypothesisComments: %w", err)
	}
	total, err := s.store.CountCommentsByHypothesis(ctx, hypothesisID)
	if err != nil {
		return nil, 0, fmt.Errorf("registry.ListHypothesisComments: %w", err)
	}
	return cs, total, nil
}

func (s *Service) requireAgent(ctx context.Context, agentID string) error {
	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		return err
	}
	if agent == nil {
		return fmt.Errorf("%w: %s", db.ErrUnknownAgent, agentID)
	}
	return nil
}
