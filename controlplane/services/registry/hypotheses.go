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
// A human submitting from the UI passes author instead of agentID. This is the one place the
// exactly-one-of rule is applied (domain.ClassifyHypothesisOrigin): an invalid pair is an error
// here, never a value coerced into a row for some later read to interpret.
func (s *Service) RegisterHypothesis(ctx context.Context, agentID, author, platformExperimentID, text string, links []domain.LinkSpec, distinctFrom []string) (h *domain.Hypothesis, out []domain.Link, alreadyExisted bool, err error) {
	source, err := domain.ClassifyHypothesisOrigin(agentID, author)
	if err != nil {
		return nil, nil, false, fmt.Errorf("registry.RegisterHypothesis: %w", err)
	}

	hypothesisPolicy, _, found, err := s.store.GetPlatformExperimentSubmitPolicies(ctx, platformExperimentID)
	if err != nil {
		return nil, nil, false, fmt.Errorf("registry.RegisterHypothesis: %w", err)
	}
	if !found {
		return nil, nil, false, fmt.Errorf("%w: %s", ErrUnknownPlatformExperiment, platformExperimentID)
	}
	switch source {
	case domain.HypothesisSourceHuman:
		if !hypothesisPolicy.AllowsHuman() {
			return nil, nil, false, fmt.Errorf("%w: platform experiment %s accepts hypotheses from agents only — this is a human submission (author=%q); read the experiment's hypothesis_submit_policy before retrying, a human identity cannot submit hypotheses here",
				ErrHypothesisSubmitterNotAllowed, platformExperimentID, author)
		}
	case domain.HypothesisSourceAgent:
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
			Source: source, AgentID: agentID, Author: author,
			PlatformExperimentID: platformExperimentID, Text: text, Links: links, DistinctFrom: distinctFrom,
			Stamp: stamp,
		})
		if err != nil || !announce {
			return nil, err
		}
		return []db.Event{{Kind: db.EventHypothesisNew, Subject: h.ID, Value: string(h.Source),
			PlatformExperimentID: h.PlatformExperimentID, AgentID: h.AgentID}}, nil
	})
	if err != nil {
		return nil, nil, false, fmt.Errorf("registry.RegisterHypothesis: %w", err)
	}
	s.logger.Info("hypothesis registered",
		zap.String("id", h.ID), zap.String("agent", agentID), zap.String("author", author),
		zap.String("platform_experiment_id", platformExperimentID), zap.Bool("already_existed", alreadyExisted))
	return h, out, alreadyExisted, nil
}

// AddHypothesisLink adds one outgoing link from an existing hypothesis on behalf of its registering
// agent, or of anyone when a human submitted it. Re-adding an existing link changes nothing. Returns
// the hypothesis's outgoing links.
func (s *Service) AddHypothesisLink(ctx context.Context, id, agentID, author string, spec domain.LinkSpec) ([]domain.Link, error) {
	source, err := domain.ClassifyHypothesisOrigin(agentID, author)
	if err != nil {
		return nil, fmt.Errorf("registry.AddHypothesisLink: %w", err)
	}
	if source == domain.HypothesisSourceAgent {
		if err := s.requireAgent(ctx, agentID); err != nil {
			return nil, fmt.Errorf("registry.AddHypothesisLink: %w", err)
		}
	}
	var result *graphdb.LinkResult
	err = s.events.Emit(ctx, func(stamp db.Stamp) ([]db.Event, error) {
		var err error
		result, err = s.graph.AddLink(ctx, id, agentID, author, spec, stamp)
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
		zap.String("agent", agentID), zap.String("author", author), zap.Bool("created", result.Created))
	return result.Out, nil
}

// GetHypothesis returns a single hypothesis by ID, nil when there is none.
func (s *Service) GetHypothesis(ctx context.Context, id string) (*domain.Hypothesis, error) {
	h, err := s.graph.GetHypothesis(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("registry.GetHypothesis: %w", err)
	}
	return h, nil
}

// GetHypothesisLinks returns the links touching one hypothesis, both directions.
func (s *Service) GetHypothesisLinks(ctx context.Context, id string) (domain.HypothesisLinks, error) {
	links, err := s.graph.Links(ctx, id)
	if err != nil {
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

// HypothesisGraph returns the whole hypothesis graph of one platform experiment.
func (s *Service) HypothesisGraph(ctx context.Context, platformExperimentID string) ([]*domain.HypothesisListItem, []domain.GraphEdge, error) {
	_, _, found, err := s.store.GetPlatformExperimentSubmitPolicies(ctx, platformExperimentID)
	if err != nil {
		return nil, nil, fmt.Errorf("registry.HypothesisGraph: %w", err)
	}
	if !found {
		return nil, nil, fmt.Errorf("%w: %s", ErrUnknownPlatformExperiment, platformExperimentID)
	}
	nodes, edges, err := s.graph.ExperimentGraph(ctx, platformExperimentID)
	if err != nil {
		return nil, nil, fmt.Errorf("registry.HypothesisGraph: %w", err)
	}
	if err := s.addActivity(ctx, nodes); err != nil {
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

// HypothesisEvidence is a hypothesis with what was found for it.
type HypothesisEvidence struct {
	*domain.Hypothesis
	// Finding is the newest finding filed against it, nil when none was.
	Finding *domain.HypothesisFinding `json:"finding,omitempty"`
	// BestJob is the job testing it that holds the best raw value on the platform experiment's
	// first ranking metric, nil when none reported it.
	BestJob *BestJob `json:"best_job,omitempty"`
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
	ids := make([]string, len(hypotheses))
	for i, h := range hypotheses {
		ids[i] = h.ID
	}
	newest, err := s.store.NewestFindings(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("registry.Findings: %w", err)
	}
	jobs, err := s.store.ListExperimentsTesting(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("registry.Findings: %w", err)
	}
	if err := metricsdb.PopulateJobMetrics(ctx, s.metricsDBURL, s.store.GetPlatformExperimentMetrics, jobs); err != nil {
		return nil, fmt.Errorf("registry.Findings: %w", err)
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
		return nil, fmt.Errorf("registry.Findings: %w", err)
	}
	out := []*HypothesisEvidence{}
	for _, h := range hypotheses {
		e := &HypothesisEvidence{Hypothesis: h.Hypothesis, Finding: newest[h.ID]}
		if job := best[h.ID]; job != nil {
			e.BestJob = &BestJob{ID: job.ID, Metric: job.Metric, Summary: summaries[job.ID]}
		}
		if h.Status != domain.HypothesisOpen || e.Finding != nil || e.BestJob != nil {
			out = append(out, e)
		}
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
// own verdict on its own claim, or any agent's verdict on an unowned human row (see
// domain.HypothesisStatus). Ownership is checked in the same graph transaction as the write.
func (s *Service) SetHypothesisStatus(ctx context.Context, id, callerAgentID string, status domain.HypothesisStatus) (*domain.Hypothesis, error) {
	if !domain.ValidHypothesisStatus(status) {
		return nil, fmt.Errorf("registry.SetHypothesisStatus: invalid status %q", status)
	}
	var h *domain.Hypothesis
	var announced bool
	err := s.events.Emit(ctx, func(stamp db.Stamp) ([]db.Event, error) {
		var err error
		h, announced, err = s.graph.UpdateStatus(ctx, id, callerAgentID, status, stamp)
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
	return fs, total, nil
}

// AddHypothesisComment records a freeform, job-independent note against a hypothesis — see
// domain.HypothesisComment for how this differs from a finding.
// A human commenting from the UI passes author instead of agentID, under the same exactly-one-of
// rule registration applies.
func (s *Service) AddHypothesisComment(ctx context.Context, hypothesisID, agentID, author, text string) (*domain.HypothesisComment, error) {
	source, err := domain.ClassifyHypothesisOrigin(agentID, author)
	if err != nil {
		return nil, fmt.Errorf("registry.AddHypothesisComment: %w", err)
	}
	h, err := s.graph.GetHypothesis(ctx, hypothesisID)
	if err != nil {
		return nil, fmt.Errorf("registry.AddHypothesisComment: %w", err)
	}
	if h == nil {
		return nil, fmt.Errorf("registry.AddHypothesisComment: %w: %s", ErrHypothesisNotFound, hypothesisID)
	}
	c, err := s.store.CreateHypothesisComment(ctx, hypothesisID, h.PlatformExperimentID, source, agentID, author, text)
	if err != nil {
		return nil, fmt.Errorf("registry.AddHypothesisComment: %w", err)
	}
	s.logger.Info("hypothesis comment added",
		zap.String("hypothesis_id", hypothesisID), zap.String("agent", agentID), zap.String("author", author))
	return c, nil
}

// ListHypothesisComments returns one page of the comments filed against a hypothesis, oldest
// first, alongside the full count the page was taken from.
func (s *Service) ListHypothesisComments(ctx context.Context, hypothesisID string, limit, offset int) ([]*domain.HypothesisComment, int, error) {
	cs, err := s.store.ListCommentsByHypothesis(ctx, hypothesisID, limit, offset)
	if err != nil {
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
