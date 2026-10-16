package registry

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strings"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/api"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/graphdb"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
)

var (
	// ErrHypothesisNotFound and ErrHypothesisNotOwner let the HTTP layer answer 404 and 403.
	ErrHypothesisNotFound = graphdb.ErrNotFound
	ErrHypothesisNotOwner = graphdb.ErrNotOwner
	// ErrBaselineRegistersNoHypothesis is a registration for an agent that measures the baseline.
	ErrBaselineRegistersNoHypothesis = errors.New("an agent of role baseline registers no hypothesis: the baseline is the objective's own measurement and no claim, and its job is submitted without one")
	// ErrUnknownPlatformExperiment is a registration naming a platform experiment that does not exist.
	ErrUnknownPlatformExperiment = errors.New("unknown platform_experiment_id")
)

// RegisterHypothesis registers a new hypothesis within a platform experiment, placed in the graph
// by links (at least one builds_on), or returns the existing one (alreadyExisted=true, links not
// applied) if an equivalent one by normalized text is already registered in that platform
// experiment. The returned links are the hypothesis's outgoing links. While it reads close to
// hypotheses not named in distinctFrom or a contradicts/related link, nothing is created and the
// error wraps a *graphdb.SimilarError listing them, unless actor is a signed-in person: theirs is
// registered and similar lists them.
//
// agentID is the agent whose hypothesis it is, empty for one no agent owns. actor is the
// credential registering it.
func (s *Service) RegisterHypothesis(ctx context.Context, agentID string, actor domain.Actor, platformExperimentID, text string, links []domain.LinkSpec, distinctFrom []string) (h *domain.Hypothesis, out []domain.Link, alreadyExisted bool, similar []domain.SimilarHypothesis, err error) {
	found, err := s.store.PlatformExperimentExists(ctx, platformExperimentID)
	if err != nil {
		return nil, nil, false, nil, fmt.Errorf("registry.RegisterHypothesis: %w", err)
	}
	if !found {
		return nil, nil, false, nil, fmt.Errorf("%w: %s", ErrUnknownPlatformExperiment, platformExperimentID)
	}
	if agentID != "" {
		if err := s.requireAgent(ctx, agentID); err != nil {
			return nil, nil, false, nil, fmt.Errorf("registry.RegisterHypothesis: %w", err)
		}
		signup, err := s.store.GetAgentSignup(ctx, platformExperimentID, agentID)
		if err != nil {
			return nil, nil, false, nil, fmt.Errorf("registry.RegisterHypothesis: %w", err)
		}
		if signup == nil {
			return nil, nil, false, nil, db.ErrNotSignedUp
		}
		if signup.Role == domain.SignupRoleBaseline {
			return nil, nil, false, nil, ErrBaselineRegistersNoHypothesis
		}
	}

	err = s.events.Emit(ctx, func(stamp domain.Stamp) ([]domain.Event, error) {
		if err := stamp.RequireOpen(platformExperimentID); err != nil {
			return nil, err
		}
		var announce bool
		h, out, alreadyExisted, announce, similar, err = s.graph.Register(ctx, graphdb.NewHypothesis{
			AgentID: agentID, Actor: actor,
			PlatformExperimentID: platformExperimentID, Text: text, Links: links, DistinctFrom: distinctFrom,
			Stamp: stamp,
		})
		if err != nil || !announce {
			return nil, err
		}
		return []domain.Event{{Kind: domain.EventHypothesisNew, Subject: h.ID, Value: string(h.Actor.Kind),
			PlatformExperimentID: h.PlatformExperimentID, AgentID: h.AgentID}}, nil
	})
	var held *graphdb.SimilarError
	if errors.As(err, &held) {
		similar = held.Similar
	}
	named := make([]*domain.Actor, len(similar))
	for i := range similar {
		named[i] = &similar[i].Actor
	}
	if nameErr := s.store.NameActors(ctx, named); nameErr != nil {
		return nil, nil, false, nil, fmt.Errorf("registry.RegisterHypothesis: %w", nameErr)
	}
	if err != nil {
		return nil, nil, false, nil, fmt.Errorf("registry.RegisterHypothesis: %w", err)
	}
	if err := s.store.NameActors(ctx, append(linkActors(out), hypothesisActors(h)...)); err != nil {
		return nil, nil, false, nil, fmt.Errorf("registry.RegisterHypothesis: %w", err)
	}
	s.logger.Info("hypothesis registered",
		zap.String("id", h.ID), zap.String("agent", agentID), zap.String("actor_kind", string(actor.Kind)), zap.String("actor_id", actor.ID),
		zap.String("platform_experiment_id", platformExperimentID), zap.Bool("already_existed", alreadyExisted))
	return h, out, alreadyExisted, similar, nil
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
	from, err := s.graph.GetHypothesis(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("registry.AddHypothesisLink: %w", err)
	}
	if from == nil {
		return nil, fmt.Errorf("registry.AddHypothesisLink: %w: %s", ErrHypothesisNotFound, id)
	}
	var result *graphdb.LinkResult
	err = s.events.Emit(ctx, func(stamp domain.Stamp) ([]domain.Event, error) {
		if err := stamp.RequireOpen(from.PlatformExperimentID); err != nil {
			return nil, err
		}
		var err error
		result, err = s.graph.AddLink(ctx, id, agentID, actor, spec, stamp)
		if err != nil || !result.Announce {
			return nil, err
		}
		return []domain.Event{{Kind: domain.EventHypothesisLink, Subject: id, Value: string(spec.Type), Detail: spec.To,
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

// Implementations returns the branches a hypothesis was run on, each with its agent and newest
// commit.
func (s *Service) Implementations(ctx context.Context, hypothesisID string) ([]domain.Implementation, error) {
	implementations, err := s.store.Implementations(ctx, hypothesisID)
	if err != nil {
		return nil, fmt.Errorf("registry.Implementations: %w", err)
	}
	return implementations, nil
}

// BaseRef returns the commit a new implementation of h starts from: the one the newest completed
// job of its first builds_on parent ran, whichever agent's. A parent with no completed job
// passes the question to its own first parent, and the objective answers with the commit of the
// declared baseline job. Empty when the chain ends at the objective and no baseline is declared.
func (s *Service) BaseRef(ctx context.Context, h *domain.Hypothesis) (string, error) {
	id := h.ID
	for {
		parent, err := s.graph.FirstParent(ctx, id)
		if err != nil {
			return "", fmt.Errorf("registry.BaseRef: %w", err)
		}
		if parent == "" {
			return "", fmt.Errorf("registry.BaseRef: hypothesis %s builds on nothing", id)
		}
		if parent == h.PlatformExperimentID {
			ref, err := s.store.BaselineCodeRef(ctx, parent)
			if err != nil {
				return "", fmt.Errorf("registry.BaseRef: %w", err)
			}
			return ref, nil
		}
		ref, err := s.store.LastCompletedCodeRef(ctx, parent)
		if err != nil {
			return "", fmt.Errorf("registry.BaseRef: %w", err)
		}
		if ref != "" {
			return ref, nil
		}
		id = parent
	}
}

// ListHypotheses returns one page of hypotheses, the open ideas of people first, then the most
// recent, each carrying its place in the graph and its finding/comment counts. The caller states the page.
// ListHypotheses returns the hypotheses matching f, each with what was found for it as its graph
// node carries it, judged by its own platform experiment's ranking metric.
func (s *Service) ListHypotheses(ctx context.Context, f graphdb.ListFilter) ([]*GraphNode, error) {
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
	ids := make([]string, len(items))
	for i, item := range items {
		ids[i] = item.ID
	}
	working, err := s.store.Working(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("registry.ListHypotheses: %w", err)
	}
	for _, item := range items {
		item.Working = working[item.ID]
	}
	byExperiment := map[string][]string{}
	for _, item := range items {
		byExperiment[item.PlatformExperimentID] = append(byExperiment[item.PlatformExperimentID], item.ID)
	}
	evidence := map[string]Evidence{}
	for pe, peIDs := range byExperiment {
		metrics, _, err := s.store.GetPlatformExperimentMetrics(ctx, pe)
		if err != nil {
			return nil, fmt.Errorf("registry.ListHypotheses: %w", err)
		}
		found, err := s.evidence(ctx, metrics, peIDs)
		if err != nil {
			return nil, fmt.Errorf("registry.ListHypotheses: %w", err)
		}
		maps.Copy(evidence, found)
	}
	nodes := make([]*GraphNode, len(items))
	actors := itemActors(items)
	for i, item := range items {
		nodes[i] = &GraphNode{HypothesisListItem: item, Evidence: evidence[item.ID]}
		if f := nodes[i].Finding; f != nil {
			actors = append(actors, &f.Actor)
		}
	}
	if err := s.store.NameActors(ctx, actors); err != nil {
		return nil, fmt.Errorf("registry.ListHypotheses: %w", err)
	}
	return nodes, nil
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

// HypothesisGraph is one platform experiment's whole hypothesis graph, rooted at its objective.
type HypothesisGraph struct {
	Root string `json:"root"`
	// Objective is what the baseline's jobs found, they test no hypothesis and hang on the root,
	// and what was said on the objective itself.
	Objective ObjectiveEvidence  `json:"objective"`
	Nodes     []*GraphNode       `json:"nodes"`
	Edges     []domain.GraphEdge `json:"edges"`
}

// ObjectiveEvidence is the Evidence of the objective with the comments on it: CommentCount of
// them, and the newest objectiveComments, oldest first.
type ObjectiveEvidence struct {
	Evidence
	Comments     []*domain.HypothesisComment `json:"comments"`
	CommentCount int                         `json:"comment_count"`
}

// objectiveComments is how many of the newest comments on the objective its graph carries.
const objectiveComments = 5

// HypothesisGraph returns the whole hypothesis graph of one platform experiment, each node with
// its newest finding and its best job, and the same of the objective, whose jobs are the
// baseline's.
func (s *Service) HypothesisGraph(ctx context.Context, platformExperimentID string) (*HypothesisGraph, error) {
	metrics, found, err := s.store.GetPlatformExperimentMetrics(ctx, platformExperimentID)
	if err != nil {
		return nil, fmt.Errorf("registry.HypothesisGraph: %w", err)
	}
	if !found {
		return nil, fmt.Errorf("%w: %s", ErrUnknownPlatformExperiment, platformExperimentID)
	}
	items, edges, err := s.graph.ExperimentGraph(ctx, platformExperimentID)
	if err != nil {
		return nil, fmt.Errorf("registry.HypothesisGraph: %w", err)
	}
	if err := s.addActivity(ctx, items); err != nil {
		return nil, fmt.Errorf("registry.HypothesisGraph: %w", err)
	}
	ids := make([]string, len(items), len(items)+1)
	for i, item := range items {
		ids[i] = item.ID
	}
	evidence, err := s.evidence(ctx, metrics, append(ids, platformExperimentID))
	if err != nil {
		return nil, fmt.Errorf("registry.HypothesisGraph: %w", err)
	}
	nodes := make([]*GraphNode, len(items))
	actors := itemActors(items)
	for i, item := range items {
		nodes[i] = &GraphNode{HypothesisListItem: item, Evidence: evidence[item.ID]}
		if f := nodes[i].Finding; f != nil {
			actors = append(actors, &f.Actor)
		}
	}
	objective := ObjectiveEvidence{Evidence: evidence[platformExperimentID]}
	if objective.Finding != nil {
		actors = append(actors, &objective.Finding.Actor)
	}
	if objective.CommentCount, err = s.store.CountCommentsByHypothesis(ctx, platformExperimentID); err != nil {
		return nil, fmt.Errorf("registry.HypothesisGraph: %w", err)
	}
	if objective.Comments, err = s.store.ListCommentsByHypothesis(ctx, platformExperimentID, objectiveComments, max(0, objective.CommentCount-objectiveComments)); err != nil {
		return nil, fmt.Errorf("registry.HypothesisGraph: %w", err)
	}
	for _, c := range objective.Comments {
		actors = append(actors, &c.Actor)
	}
	for i := range edges {
		actors = append(actors, &edges[i].Actor)
	}
	if err := s.store.NameActors(ctx, actors); err != nil {
		return nil, fmt.Errorf("registry.HypothesisGraph: %w", err)
	}
	return &HypothesisGraph{Root: platformExperimentID, Objective: objective, Nodes: nodes, Edges: edges}, nil
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
	// BestJob is the completed job testing it that holds the best raw value on the platform experiment's
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

// Findings returns what a platform experiment has found: among its first maxFindings hypotheses
// as listed, each one that holds a verdict, a finding or a job with a metric, with its newest
// finding and its best job, which carries its own summary.
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
	ids := make([]string, len(hypotheses))
	for i, h := range hypotheses {
		ids[i] = h.ID
	}
	evidence, err := s.evidence(ctx, metrics, ids)
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

// evidence returns, by id, the newest finding of each of the hypotheses ids names and the job
// testing it that did best on the first ranking metric of metrics, with that job's own summary.
// A platform experiment's id names its objective, whose jobs are the baseline's. It is a fixed
// number of reads however many ids are given.
func (s *Service) evidence(ctx context.Context, metrics []domain.MetricDefinition, ids []string) (map[string]Evidence, error) {
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
		if job.Status != domain.StatusCompleted || job.Metric == nil || job.Metric.Basis != "raw" {
			continue
		}
		if cur := best[job.HypothesisID]; cur != nil {
			better := job.Metric.Value > cur.Metric.Value
			if primary.Direction == "minimize" {
				better = job.Metric.Value < cur.Metric.Value
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
	if current == nil {
		return nil, fmt.Errorf("registry.SetHypothesisStatus: %w: %s", ErrHypothesisNotFound, id)
	}
	// Whose verdict it is comes first and is the graph's to refuse, in the write below: the
	// evidence is asked only of an agent that may give the verdict at all.
	mayJudge := current.AgentID == "" || current.AgentID == callerAgentID
	// A verdict that stands with its basis noted is said again and changes nothing. One that
	// stands without it lost the note to a failed write, and this call writes it.
	note := mayJudge && (status == domain.HypothesisConfirmed || status == domain.HypothesisRefuted)
	var h *domain.Hypothesis
	var announced bool
	var basis string
	err = s.events.Emit(ctx, func(stamp domain.Stamp) ([]domain.Event, error) {
		if err := stamp.RequireOpen(current.PlatformExperimentID); err != nil {
			return nil, err
		}
		if note && current.Status == status {
			noted, err := s.basisNoted(ctx, current)
			if err != nil {
				return nil, err
			}
			note = !noted
		}
		if mayJudge && (current.Status != status || note) {
			var err error
			if basis, err = s.supported(ctx, current, verdict); err != nil {
				return nil, err
			}
		}
		var err error
		h, announced, err = s.graph.UpdateStatus(ctx, id, callerAgentID, actor, status, stamp)
		if err != nil || !announced {
			return nil, err
		}
		return []domain.Event{{Kind: domain.EventHypothesisStatus, Subject: h.ID, Value: string(h.Status),
			PlatformExperimentID: h.PlatformExperimentID, AgentID: h.AgentID}}, nil
	})
	if err != nil {
		return nil, fmt.Errorf("registry.SetHypothesisStatus: %w", err)
	}
	s.logger.Info("hypothesis status updated",
		zap.String("id", id), zap.String("agent", callerAgentID), zap.String("status", string(status)), zap.Bool("announced", announced))
	if note {
		if _, err := s.AddHypothesisComment(ctx, h.ID, callerAgentID, actor, basis); err != nil {
			return nil, fmt.Errorf("registry.SetHypothesisStatus: the verdict is set and its basis could not be noted: %w", err)
		}
	}
	if err := s.store.NameActors(ctx, hypothesisActors(h)); err != nil {
		return nil, fmt.Errorf("registry.SetHypothesisStatus: %w", err)
	}
	return h, nil
}

// Changer is who asks to edit or remove a hypothesis: the signed-in person behind the request,
// empty when none is, and whether they own the org the hypothesis lives in.
type Changer struct {
	UserID   string
	OrgOwner bool
}

// ChangeRefusal says why caller may not edit, or with remove set remove, h, nil when they may. A
// hypothesis a person wrote for no agent is theirs to change, and also an owner's of its org to
// remove. An agent's hypothesis, and one written with a credential no person signed in with, is
// nobody's to change.
func ChangeRefusal(h *domain.Hypothesis, caller Changer, remove bool) *api.Refusal {
	verb := "edit"
	if remove {
		verb = "remove"
	}
	switch {
	case caller.UserID == "":
		return api.Refuse(http.StatusForbidden, "person_only", "only the signed-in person who wrote an idea may "+verb+" it, and this request carries no person")
	case h.AgentID != "":
		return api.Refuse(http.StatusForbidden, "agent_hypothesis", fmt.Sprintf("hypothesis %s is agent %s's, and an agent's hypotheses are its record: nobody may %s one. Add a comment to it instead", h.ID, h.AgentID, verb))
	case h.Actor.Kind != domain.ActorUser:
		return api.Refuse(http.StatusForbidden, "not_a_persons_idea", fmt.Sprintf("hypothesis %s was registered with a %s credential and no person wrote it, so nobody may %s it. Add a comment to it instead", h.ID, h.Actor.Kind, verb))
	case h.Actor.ID == caller.UserID, remove && caller.OrgOwner:
		return nil
	case remove:
		return api.Refuse(http.StatusForbidden, "not_the_author", "only the person who wrote this idea, or an owner of its org, may remove it. Add a comment to it instead")
	default:
		return api.Refuse(http.StatusForbidden, "not_the_author", "only the person who wrote this idea may edit it. Add a comment to it instead")
	}
}

// InRecordError refuses a change to a hypothesis an agent has acted on: Jobs tested it, or Linked
// hypotheses link to it.
type InRecordError struct {
	Verb   string
	Jobs   []string
	Linked []domain.Link
}

// maxNamed bounds how many jobs and links an InRecordError names.
const maxNamed = 10

func (e *InRecordError) Error() string {
	var why []string
	if len(e.Jobs) > 0 {
		why = append(why, named("job", e.Jobs)+" tested it")
	}
	if len(e.Linked) > 0 {
		links := make([]string, len(e.Linked))
		for i, l := range e.Linked {
			links[i] = fmt.Sprintf("%s (%s)", l.HypothesisID, l.Type)
		}
		why = append(why, named("hypothesis", links)+" link to it")
	}
	return fmt.Sprintf("this idea is part of the record and can no longer be %s: %s. Add a comment to it instead",
		e.Verb, strings.Join(why, ", and "))
}

func named(what string, ids []string) string {
	shown := ids[:min(len(ids), maxNamed)]
	out := what + " " + strings.Join(shown, ", ")
	if len(ids) > 1 {
		out = what + "s " + strings.Join(shown, ", ")
	}
	if more := len(ids) - len(shown); more > 0 {
		out += fmt.Sprintf(" and %d more", more)
	}
	return out
}

// untouched refuses with an *InRecordError while a job tests h or another hypothesis links to
// it. The graph checks the links again in the write that changes h.
func (s *Service) untouched(ctx context.Context, h *domain.Hypothesis, verb string) error {
	jobs, err := s.store.ListExperimentsTesting(ctx, []string{h.ID})
	if err != nil {
		return err
	}
	links, err := s.graph.Links(ctx, h.ID)
	if err != nil {
		return err
	}
	if len(jobs) == 0 && len(links.In) == 0 {
		return nil
	}
	ids := make([]string, len(jobs))
	for i, job := range jobs {
		ids[i] = job.ID
	}
	return &InRecordError{Verb: verb, Jobs: ids, Linked: links.In}
}

// inRecord turns the graph's refusal of a linked hypothesis into an *InRecordError.
func inRecord(err error, verb string) error {
	var linked *graphdb.LinkedError
	if errors.As(err, &linked) {
		return &InRecordError{Verb: verb, Linked: linked.In}
	}
	return err
}

// EditHypothesis replaces the text of hypothesis id while no job tests it and no other hypothesis
// links to it. Whether the caller may is ChangeRefusal's. No event announces it: an agent reads
// the new text wherever it reads the hypothesis.
func (s *Service) EditHypothesis(ctx context.Context, id, text string) (*domain.Hypothesis, error) {
	current, err := s.graph.GetHypothesis(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("registry.EditHypothesis: %w", err)
	}
	if current == nil {
		return nil, fmt.Errorf("registry.EditHypothesis: %w: %s", ErrHypothesisNotFound, id)
	}
	var h *domain.Hypothesis
	err = s.events.Emit(ctx, func(stamp domain.Stamp) ([]domain.Event, error) {
		if err := stamp.RequireOpen(current.PlatformExperimentID); err != nil {
			return nil, err
		}
		if err := s.untouched(ctx, current, "edited"); err != nil {
			return nil, err
		}
		var err error
		h, err = s.graph.EditText(ctx, id, text)
		return nil, inRecord(err, "edited")
	})
	if err != nil {
		return nil, fmt.Errorf("registry.EditHypothesis: %w", err)
	}
	if err := s.store.NameActors(ctx, hypothesisActors(h)); err != nil {
		return nil, fmt.Errorf("registry.EditHypothesis: %w", err)
	}
	s.logger.Info("hypothesis edited", zap.String("id", id))
	return h, nil
}

// RemoveHypothesis deletes hypothesis id, its outgoing links and its comments while no job tests
// it and no other hypothesis links to it. Whether the caller may is ChangeRefusal's.
func (s *Service) RemoveHypothesis(ctx context.Context, id string) error {
	current, err := s.graph.GetHypothesis(ctx, id)
	if err != nil {
		return fmt.Errorf("registry.RemoveHypothesis: %w", err)
	}
	if current == nil {
		return fmt.Errorf("registry.RemoveHypothesis: %w: %s", ErrHypothesisNotFound, id)
	}
	err = s.events.Emit(ctx, func(stamp domain.Stamp) ([]domain.Event, error) {
		if err := stamp.RequireOpen(current.PlatformExperimentID); err != nil {
			return nil, err
		}
		if err := s.untouched(ctx, current, "removed"); err != nil {
			return nil, err
		}
		return nil, inRecord(s.graph.Remove(ctx, id), "removed")
	})
	if err != nil {
		return fmt.Errorf("registry.RemoveHypothesis: %w", err)
	}
	if err := s.store.DeleteHypothesisComments(ctx, id); err != nil {
		return fmt.Errorf("registry.RemoveHypothesis: the hypothesis is removed and its comments are not: %w", err)
	}
	s.logger.Info("hypothesis removed", zap.String("id", id), zap.String("platform_experiment_id", current.PlatformExperimentID))
	return nil
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

// AddHypothesisComment records a freeform, job-independent note against a hypothesis, or against
// the objective when hypothesisID is a platform experiment's id — see domain.HypothesisComment for
// how this differs from a finding. agentID is the agent whose note it is, empty for one no agent
// owns, and actor the credential writing it.
func (s *Service) AddHypothesisComment(ctx context.Context, hypothesisID, agentID string, actor domain.Actor, text string) (*domain.HypothesisComment, error) {
	platformExperimentID, err := s.CommentTarget(ctx, hypothesisID)
	if err != nil {
		return nil, fmt.Errorf("registry.AddHypothesisComment: %w", err)
	}
	c, err := s.store.CreateHypothesisComment(ctx, hypothesisID, platformExperimentID, agentID, actor, text)
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

// CommentTarget is the platform experiment a comment on id lands in: the hypothesis's, or id
// itself when it names a platform experiment, whose objective is the root of its graph and holds
// comments like any hypothesis, under its id.
func (s *Service) CommentTarget(ctx context.Context, id string) (string, error) {
	h, err := s.graph.GetHypothesis(ctx, id)
	if err != nil {
		return "", err
	}
	if h != nil {
		return h.PlatformExperimentID, nil
	}
	found, err := s.store.PlatformExperimentExists(ctx, id)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("%w: %s", ErrHypothesisNotFound, id)
	}
	return id, nil
}

// ListHypothesisComments returns one page of the comments filed against a hypothesis, or against
// the objective under its platform experiment's id, oldest first, alongside the full count the
// page was taken from.
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
