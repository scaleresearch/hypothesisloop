package quota

import (
	"context"
	"fmt"
	"sort"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
)

// AgentStanding is one agent's best value on one declared metric, and the experiment that
// produced it. The experiment id is the whole point: a number alone says a result was measured
// once, while the experiment it came from carries the code_ref — the patch someone can actually
// pick up and reuse.
type AgentStanding struct {
	Rank    int     `json:"rank"`
	AgentID string  `json:"agent_id"`
	Best    float64 `json:"best"`
	// Basis is always "raw": only raw-basis values are ever ranked here. Present so the UI can
	// show it next to the value without a second lookup.
	Basis        string `json:"basis"`
	ExperimentID string `json:"experiment_id,omitempty"`
	CodeRef      string `json:"code_ref,omitempty"`
	// HypothesisID and Env say what that experiment tested and with which settings.
	HypothesisID string            `json:"hypothesis_id,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
}

// MetricStandings is the full ranking on a single declared metric, best-first.
type MetricStandings struct {
	Metric    string          `json:"metric"`
	Direction string          `json:"direction"`
	Standings []AgentStanding `json:"standings"`
	// NonRawBasisAgents lists agents that reported at least one non-"raw"-basis value for this
	// metric (e.g. a rescaled loss target). That value is never blended into Standings — if the
	// same agent also reported a "raw" value it is ranked on that alone. Surfaced so the UI can
	// flag it rather than the discrepancy staying invisible.
	NonRawBasisAgents []string `json:"non_raw_basis_agents,omitempty"`
}

// AgentBest is one agent's best "raw"-basis value on a metric and the job that set it.
type AgentBest struct {
	AgentID      string  `json:"agent_id"`
	Best         float64 `json:"best"`
	ExperimentID string  `json:"experiment_id,omitempty"`
}

// MetricBests is what every agent measured on one declared metric, whatever the metric's role or
// the agent's: Standings say who ranks, this says what was reported.
type MetricBests struct {
	Metric            string      `json:"metric"`
	Agents            []AgentBest `json:"agents"`
	NonRawBasisAgents []string    `json:"non_raw_basis_agents,omitempty"`
}

// PlatformExperimentResults is what a finished run amounts to: the operator's narrative plus the
// standings on every ranking metric and each agent's best on every declared one. The standings are computed from the metrics store on every
// read rather than frozen into PostgreSQL at close — one source of truth for a number, and a
// result stays correct if a late metric lands or a metric is re-queried over a different window.
type PlatformExperimentResults struct {
	PlatformExperimentID string            `json:"platform_experiment_id"`
	Name                 string            `json:"name"`
	Status               string            `json:"status"`
	Summary              string            `json:"summary,omitempty"`
	Metrics              []MetricStandings `json:"metrics"`
	Bests                []MetricBests     `json:"bests"`
}

// Results derives the standings for a platform experiment from the metrics store.
func (s *PlatformExperimentsService) Results(ctx context.Context, id string) (*PlatformExperimentResults, error) {
	pe, err := s.store.GetPlatformExperiment(ctx, id)
	if err != nil {
		return nil, err
	}
	if pe == nil {
		return nil, fmt.Errorf("not_found")
	}

	ineligible, err := s.constraintIneligibleAgents(ctx, pe, pe.Metrics)
	if err != nil {
		return nil, err
	}

	out := &PlatformExperimentResults{
		PlatformExperimentID: pe.ID,
		Name:                 pe.Name,
		Status:               string(pe.Status),
		Summary:              pe.Summary,
		Metrics:              make([]MetricStandings, 0, len(pe.Metrics)),
		Bests:                make([]MetricBests, 0, len(pe.Metrics)),
	}
	for _, metric := range pe.Metrics {
		best, nonRawAgents, err := metricsdb.BestPerAgentOnMetric(ctx, s.metricsDBURL, pe, metric)
		if err != nil {
			return nil, fmt.Errorf("results: %w", err)
		}
		bests := MetricBests{Metric: metric.Key, Agents: make([]AgentBest, 0, len(best)), NonRawBasisAgents: nonRawAgents}
		for agentID, b := range best {
			bests.Agents = append(bests.Agents, AgentBest{AgentID: agentID, Best: b.Value, ExperimentID: b.ExperimentID})
		}
		sort.Slice(bests.Agents, func(i, j int) bool { return bests.Agents[i].AgentID < bests.Agents[j].AgentID })
		out.Bests = append(out.Bests, bests)
		if metric.EffectiveRole() != domain.MetricRoleRanking {
			continue
		}
		eligible, err := s.eligibleStandings(ctx, pe, metric, best, ineligible)
		if err != nil {
			return nil, err
		}
		out.Metrics = append(out.Metrics, MetricStandings{
			Metric:            metric.Key,
			Direction:         metric.Direction,
			Standings:         eligible,
			NonRawBasisAgents: nonRawAgents,
		})
	}
	return out, nil
}

// constraintIneligibleAgents returns the set of agents whose best value on any declared
// constraint metric violates its bound, or who never reported that metric at all — a
// constraint must be reported and satisfied, not merely absent. A job violating one is
// ineligible for standings entirely (docs: "the correctness gate"), never itself ranked.
func (s *PlatformExperimentsService) constraintIneligibleAgents(ctx context.Context, pe *domain.PlatformExperiment, metrics []domain.MetricDefinition) (map[string]bool, error) {
	ineligible := make(map[string]bool)
	for _, metric := range metrics {
		if metric.EffectiveRole() != domain.MetricRoleConstraint {
			continue
		}
		standings, err := s.standingsOnMetric(ctx, pe, metric)
		if err != nil {
			return nil, err
		}
		reported := make(map[string]bool, len(standings))
		for _, st := range standings {
			reported[st.AgentID] = true
			satisfies := st.Best >= *metric.Bound
			if metric.Direction == "minimize" {
				satisfies = st.Best <= *metric.Bound
			}
			if !satisfies {
				ineligible[st.AgentID] = true
			}
		}
		signups, err := s.store.ListSignups(ctx, pe.ID)
		if err != nil {
			return nil, err
		}
		for _, agentID := range signups {
			if !reported[agentID] {
				ineligible[agentID] = true
			}
		}
	}
	return ineligible, nil
}

// derivedTopResults ranks agents on the experiment's first declared ranking metric — the primary
// metric by convention — so an auto-closed run records the same placements a hand-closed one would.
func (s *PlatformExperimentsService) derivedTopResults(ctx context.Context, id string) ([]AgentResult, error) {
	pe, err := s.store.GetPlatformExperiment(ctx, id)
	if err != nil {
		return nil, err
	}
	if pe == nil {
		return nil, nil
	}
	primary, ok := domain.PrimaryRankingMetric(pe.Metrics)
	if !ok {
		return nil, nil
	}
	ineligible, err := s.constraintIneligibleAgents(ctx, pe, pe.Metrics)
	if err != nil {
		return nil, err
	}
	best, _, err := metricsdb.BestPerAgentOnMetric(ctx, s.metricsDBURL, pe, primary)
	if err != nil {
		return nil, fmt.Errorf("results: %w", err)
	}
	standings, err := s.eligibleStandings(ctx, pe, primary, best, ineligible)
	if err != nil {
		return nil, err
	}
	out := make([]AgentResult, 0, len(standings))
	for _, st := range standings {
		out = append(out, AgentResult{AgentID: st.AgentID, FinalMetric: st.Best})
	}
	return out, nil
}

// eligibleStandings is the ranking a run is judged by: competitors that pass every constraint,
// best-first. Results and the top-3 recorded at close both read it, so they cannot diverge.
func (s *PlatformExperimentsService) eligibleStandings(ctx context.Context, pe *domain.PlatformExperiment, metric domain.MetricDefinition, best map[string]metricsdb.AgentBest, ineligible map[string]bool) ([]AgentStanding, error) {
	standings, err := s.rank(ctx, pe, metric, best)
	if err != nil {
		return nil, err
	}
	eligible := make([]AgentStanding, 0, len(standings))
	for _, st := range standings {
		if ineligible[st.AgentID] {
			continue
		}
		eligible = append(eligible, st)
	}
	assignRanks(eligible)
	return eligible, nil
}

// standingsOnMetric ranks every agent by its best "raw"-basis value on one metric, best-first.
// The ranking itself is metricsdb.BestPerAgentOnMetric — the same call the stage ladder cuts with,
// so a run's final standings and its mid-run cuts can never disagree about who is ahead.
func (s *PlatformExperimentsService) standingsOnMetric(ctx context.Context, pe *domain.PlatformExperiment, metric domain.MetricDefinition) ([]AgentStanding, error) {
	best, _, err := metricsdb.BestPerAgentOnMetric(ctx, s.metricsDBURL, pe, metric)
	if err != nil {
		return nil, fmt.Errorf("results: %w", err)
	}
	return s.rank(ctx, pe, metric, best)
}

// rank presents the competitors' bests on one metric as standings: attaches each one's job, sorts
// and assigns ranks.
func (s *PlatformExperimentsService) rank(ctx context.Context, pe *domain.PlatformExperiment, metric domain.MetricDefinition, best map[string]metricsdb.AgentBest) ([]AgentStanding, error) {
	// Only competitors are ranked. The metrics store answers for every agent that reported —
	// which is the point, a baseline's numbers stay fully readable — so the roles are applied
	// here rather than to the query, and every caller of this function inherits it.
	competitors, err := s.store.ListSignupsByRole(ctx, pe.ID, domain.SignupRoleCompetitor)
	if err != nil {
		return nil, fmt.Errorf("results: %w", err)
	}
	isCompetitor := make(map[string]bool, len(competitors))
	for _, agentID := range competitors {
		isCompetitor[agentID] = true
	}

	standings := make([]AgentStanding, 0, len(best))
	for agentID, b := range best {
		if !isCompetitor[agentID] {
			continue
		}
		st := AgentStanding{AgentID: agentID, Best: b.Value, Basis: "raw", ExperimentID: b.ExperimentID}
		if b.ExperimentID != "" {
			exp, err := s.store.GetExperiment(ctx, b.ExperimentID)
			if err != nil {
				return nil, fmt.Errorf("results: %s: job %s: %w", metric.Key, b.ExperimentID, err)
			}
			if exp != nil {
				st.CodeRef, st.HypothesisID, st.Env = exp.CodeRef, exp.HypothesisID, exp.Job.Env
			}
		}
		standings = append(standings, st)
	}
	sort.Slice(standings, func(i, j int) bool {
		if standings[i].Best != standings[j].Best {
			if metric.Direction == "minimize" {
				return standings[i].Best < standings[j].Best
			}
			return standings[i].Best > standings[j].Best
		}
		return standings[i].AgentID < standings[j].AgentID
	})
	assignRanks(standings)
	return standings, nil
}

// assignRanks numbers standings that are sorted best-first. Agents with the same best share a
// rank and the next one counts everyone ahead of it (1, 1, 3): the order between equals is by
// agent id, which says nothing about who did better.
func assignRanks(standings []AgentStanding) {
	for i := range standings {
		standings[i].Rank = i + 1
		if i > 0 && standings[i].Best == standings[i-1].Best {
			standings[i].Rank = standings[i-1].Rank
		}
	}
}
