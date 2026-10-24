package registry

import (
	"context"
	"fmt"
	"maps"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/graphdb"
)

// GraphNode is a hypothesis in its platform experiment's graph with what was found for it.
type GraphNode struct {
	*domain.HypothesisListItem
	Evidence
	Actions []domain.HypothesisAction `json:"actions" doc:"What the reader may do to the hypothesis now, as on GET /hypotheses/{id}."`
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
		if (h.Status == domain.HypothesisOpen || h.Curated()) && e.Finding == nil && e.BestJob == nil {
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
	if err := s.metrics.PopulateJobMetrics(ctx, s.store.GetPlatformExperimentMetrics, jobs); err != nil {
		return nil, err
	}
	primary, _ := domain.PrimaryRankingMetric(metrics)
	best := map[string]*domain.Experiment{}
	for _, job := range jobs {
		if job.Status != domain.StatusCompleted || job.Metric == nil || job.Metric.Basis != "raw" {
			continue
		}
		if cur := best[job.HypothesisID]; cur != nil && !primary.Better(job.Metric.Value, cur.Metric.Value) {
			continue
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
