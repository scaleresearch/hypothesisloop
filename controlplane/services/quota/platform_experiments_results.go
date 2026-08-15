package quota

import (
	"context"
	"fmt"
	"math"
	"sort"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
)

// AgentStanding is one agent's best value on one declared metric, and the experiment that
// produced it. The experiment id is the whole point: a number alone says a result was measured
// once, while the experiment it came from carries the code_ref — the patch someone can actually
// pick up and reuse.
type AgentStanding struct {
	Rank         int     `json:"rank"`
	AgentID      string  `json:"agent_id"`
	Best         float64 `json:"best"`
	// Basis is always "raw": only raw-basis values are ever ranked here. Present so the UI can
	// show it next to the value without a second lookup.
	Basis        string  `json:"basis"`
	ExperimentID string  `json:"experiment_id,omitempty"`
	CodeRef      string  `json:"code_ref,omitempty"`
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

// PlatformExperimentResults is what a finished run amounts to: the operator's narrative plus the
// standings on every declared metric. The standings are computed from the metrics store on every
// read rather than frozen into PostgreSQL at close — one source of truth for a number, and a
// result stays correct if a late metric lands or a metric is re-queried over a different window.
type PlatformExperimentResults struct {
	PlatformExperimentID string            `json:"platform_experiment_id"`
	Name                 string            `json:"name"`
	Status               string            `json:"status"`
	Summary              string            `json:"summary,omitempty"`
	Metrics              []MetricStandings `json:"metrics"`
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

	ineligible, err := s.constraintIneligibleAgents(ctx, pe.ID, pe.Metrics)
	if err != nil {
		return nil, err
	}

	out := &PlatformExperimentResults{
		PlatformExperimentID: pe.ID,
		Name:                 pe.Name,
		Status:               string(pe.Status),
		Summary:              pe.Summary,
		Metrics:              make([]MetricStandings, 0, len(pe.Metrics)),
	}
	for _, metric := range pe.Metrics {
		if metric.EffectiveRole() != domain.MetricRoleRanking {
			continue
		}
		standings, nonRawAgents, err := s.standingsOnMetric(ctx, pe.ID, metric)
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
		for i := range eligible {
			eligible[i].Rank = i + 1
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
func (s *PlatformExperimentsService) constraintIneligibleAgents(ctx context.Context, platformExpID string, metrics []domain.MetricDefinition) (map[string]bool, error) {
	ineligible := make(map[string]bool)
	for _, metric := range metrics {
		if metric.EffectiveRole() != domain.MetricRoleConstraint {
			continue
		}
		standings, _, err := s.standingsOnMetric(ctx, platformExpID, metric)
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
		signups, err := s.store.ListSignups(ctx, platformExpID)
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
	var primary *domain.MetricDefinition
	for i := range pe.Metrics {
		if pe.Metrics[i].EffectiveRole() == domain.MetricRoleRanking {
			primary = &pe.Metrics[i]
			break
		}
	}
	if primary == nil {
		return nil, nil
	}
	standings, _, err := s.standingsOnMetric(ctx, pe.ID, *primary)
	if err != nil {
		return nil, err
	}
	out := make([]AgentResult, 0, len(standings))
	for _, st := range standings {
		out = append(out, AgentResult{AgentID: st.AgentID, FinalMetric: st.Best})
	}
	return out, nil
}

// standingsOnMetric ranks every agent by its best "raw"-basis value on one metric, best-first.
// Same direction-aware best-per-agent query the stage ladder ranks cuts with, so a run's final
// standings and its mid-run cuts can never disagree about who is ahead.
//
// Grouping by (agent_id, metric_basis) rather than agent_id alone matters: collapsing basis
// out of the query would let a rescaled/non-"raw" value silently win or lose a min/max_over_time
// aggregation against a raw one — exactly the scale mismatch that made a ~20% "win" look real
// when it was actually ~2x worse. An agent that only ever reported this metric on a non-"raw"
// basis is returned separately, flagged, never blended into the ranked list.
func (s *PlatformExperimentsService) standingsOnMetric(ctx context.Context, platformExpID string, metric domain.MetricDefinition) ([]AgentStanding, []string, error) {
	agg := "max"
	if metric.Direction == "minimize" {
		agg = "min"
	}
	promQL := fmt.Sprintf(`%s by (agent_id, metric_basis) (%s_over_time(experiment_metric_value{platform_experiment_id=%q, metric_name=%q}[30d]))`,
		agg, agg, platformExpID, metric.Key)
	samples, err := metricsdb.QueryVector(ctx, s.metricsDBURL, promQL)
	if err != nil {
		return nil, nil, fmt.Errorf("results: query %s: %w", metric.Key, err)
	}

	best := make(map[string]float64)
	nonRaw := make(map[string]bool)
	for _, sm := range samples {
		agentID := sm.Labels["agent_id"]
		if agentID == "" {
			return nil, nil, fmt.Errorf("results: query %s: result missing agent_id", metric.Key)
		}
		basis := sm.Labels["metric_basis"]
		if basis != "" && basis != "raw" {
			nonRaw[agentID] = true
			continue
		}
		// Two label-distinct series can both mean "raw": samples written before this label
		// existed carry no metric_basis at all ("") alongside samples written after that
		// explicitly say "raw". Both are eligible and must be merged with the same
		// direction-aware aggregation as everything else here, not treated as a conflict.
		if cur, exists := best[agentID]; exists {
			if metric.Direction == "minimize" {
				best[agentID] = math.Min(cur, sm.Value)
			} else {
				best[agentID] = math.Max(cur, sm.Value)
			}
			continue
		}
		best[agentID] = sm.Value
	}

	standings := make([]AgentStanding, 0, len(best))
	for agentID, value := range best {
		standings = append(standings, AgentStanding{AgentID: agentID, Best: value, Basis: "raw"})
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
	for i := range standings {
		standings[i].Rank = i + 1
	}

	nonRawAgents := make([]string, 0, len(nonRaw))
	for agentID := range nonRaw {
		nonRawAgents = append(nonRawAgents, agentID)
	}
	sort.Strings(nonRawAgents)
	return standings, nonRawAgents, nil
}
