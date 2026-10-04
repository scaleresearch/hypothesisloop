package quota

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"
	"time"

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
	// HypothesisID and Env say what that experiment tested and with which settings. A job on the
	// objective tested no hypothesis: Objective says so, as the job's own answer does
	// (domain.Experiment.OnObjective).
	HypothesisID string            `json:"hypothesis_id,omitempty"`
	Objective    bool              `json:"objective,omitempty" doc:"True when the job measured the platform experiment's objective and tested no hypothesis: it has no hypothesis_id."`
	Env          map[string]string `json:"env,omitempty"`
	RanOn        *domain.RanOn     `json:"ran_on,omitempty" doc:"Where the job ran, on results only: a reference compares with it on the same machine."`
}

// ReferenceValue is one COMPLETED job's value on a reference metric, its agent and where it ran.
type ReferenceValue struct {
	AgentID        string        `json:"agent_id"`
	Value          float64       `json:"value"`
	ExperimentID   string        `json:"experiment_id,omitempty"`
	RanOn          *domain.RanOn `json:"ran_on,omitempty"`
	ConstraintsMet string        `json:"constraints_met" enum:"true,false,unknown" doc:"Whether the job held every constraint metric: false when it broke one, unknown when it did not report one. Only a value whose constraints are met counts toward best."`
}

// MetricReference is what was measured of one outside system the goal compares against. Values
// is empty while no completed job has reported it. Best, Leader and Beaten set the leader against it: a run
// whose leader is short of a reference the goal asks to beat has not met the goal, whatever the
// standings against the control say.
type MetricReference struct {
	Metric       string           `json:"metric"`
	Direction    string           `json:"direction"`
	Values       []ReferenceValue `json:"values"`
	Best         *float64         `json:"best,omitempty" doc:"The best of values whose constraints are met, in the reference's direction, absent while there is none."`
	LeaderMetric string           `json:"leader_metric,omitempty" doc:"The first ranking metric, whose leader is set against this reference when both run in the same direction."`
	Leader       *float64         `json:"leader,omitempty" doc:"The best ranked value on leader_metric, absent while nobody ranks."`
	Beaten       *bool            `json:"beaten,omitempty" doc:"Whether leader is strictly better than best, present when both are."`
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
	// SummaryDue says that job completed and no summary was filed for it.
	SummaryDue bool `json:"summary_due"`
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
	References           []MetricReference `json:"references" doc:"Each reference metric with the value of every completed job that reported it, whether that job met the constraints, and the machine it ran on, never ranked."`
	Bests                []MetricBests     `json:"bests"`
}

// Results derives the standings for a platform experiment from the metrics store.
func (s *PlatformExperimentsService) Results(ctx context.Context, id string) (*PlatformExperimentResults, error) {
	pe, err := s.store.GetPlatformExperiment(ctx, id)
	if err != nil {
		return nil, err
	}
	if pe == nil {
		return nil, ErrPlatformExperimentNotFound
	}

	out := &PlatformExperimentResults{
		PlatformExperimentID: pe.ID,
		Name:                 pe.Name,
		Status:               string(pe.Status),
		Summary:              pe.Summary,
		Metrics:              make([]MetricStandings, 0, len(pe.Metrics)),
		References:           []MetricReference{},
		Bests:                make([]MetricBests, 0, len(pe.Metrics)),
	}
	for _, metric := range pe.Metrics {
		best, nonRawAgents, err := s.bestPerAgent(ctx, pe, metric)
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
		standings, err := s.rank(ctx, pe, metric, best)
		if err != nil {
			return nil, err
		}
		out.Metrics = append(out.Metrics, MetricStandings{
			Metric:            metric.Key,
			Direction:         metric.Direction,
			Standings:         standings,
			NonRawBasisAgents: nonRawAgents,
		})
	}
	if out.References, err = s.references(ctx, pe); err != nil {
		return nil, fmt.Errorf("results: %w", err)
	}
	// One read for every best job, whatever the number of metrics and agents.
	jobs := map[string]*domain.Experiment{}
	for _, bests := range out.Bests {
		for _, best := range bests.Agents {
			if best.ExperimentID != "" {
				jobs[best.ExperimentID] = &domain.Experiment{ID: best.ExperimentID}
			}
		}
	}
	described := make([]*domain.Experiment, 0, len(jobs))
	for _, job := range jobs {
		described = append(described, job)
	}
	if err := s.store.DescribeJobs(ctx, described); err != nil {
		return nil, fmt.Errorf("results: %w", err)
	}
	for _, bests := range out.Bests {
		for i, best := range bests.Agents {
			if job := jobs[best.ExperimentID]; job != nil {
				bests.Agents[i].SummaryDue = job.SummaryDue
			}
		}
	}
	if err := s.mergeRanOn(ctx, out); err != nil {
		return nil, fmt.Errorf("results: %w", err)
	}
	setAgainstLeader(out)
	return out, nil
}

// setAgainstLeader sets each measured reference beside rank 1 on the first ranking metric, the
// primary one by convention, when the two run in the same direction.
func setAgainstLeader(out *PlatformExperimentResults) {
	for i := range out.References {
		reference := &out.References[i]
		better := func(a, b float64) bool {
			if reference.Direction == "minimize" {
				return a < b
			}
			return a > b
		}
		for _, v := range reference.Values {
			if v.ConstraintsMet != metricsdb.ConstraintsHeld {
				continue
			}
			if reference.Best == nil || better(v.Value, *reference.Best) {
				reference.Best = &v.Value
			}
		}
		if len(out.Metrics) == 0 || out.Metrics[0].Direction != reference.Direction {
			continue
		}
		primary := out.Metrics[0]
		reference.LeaderMetric = primary.Metric
		if len(primary.Standings) == 0 || reference.Best == nil {
			continue
		}
		leader := primary.Standings[0].Best
		beaten := better(leader, *reference.Best)
		reference.Leader, reference.Beaten = &leader, &beaten
	}
}

// references is each reference metric of pe with the value of every COMPLETED job that reported
// it, whether or not the job held the constraints: a reference a job measured is listed, and only
// best weighs whether it counts.
func (s *PlatformExperimentsService) references(ctx context.Context, pe *domain.PlatformExperiment) ([]MetricReference, error) {
	out := []MetricReference{}
	if !slices.ContainsFunc(pe.Metrics, func(m domain.MetricDefinition) bool { return m.EffectiveRole() == domain.MetricRoleReference }) {
		return out, nil
	}
	completed, err := s.store.CompletedJobIDs(ctx, pe.ID)
	if err != nil {
		return nil, err
	}
	met, err := metricsdb.ConstraintsMet(ctx, s.metricsDBURL, pe, completed)
	if err != nil {
		return nil, err
	}
	for _, metric := range pe.Metrics {
		if metric.EffectiveRole() != domain.MetricRoleReference {
			continue
		}
		values, err := metricsdb.LastPerJobOnMetric(ctx, s.metricsDBURL, pe, metric, completed)
		if err != nil {
			return nil, err
		}
		reference := MetricReference{Metric: metric.Key, Direction: metric.Direction, Values: make([]ReferenceValue, 0, len(values))}
		for _, v := range values {
			reference.Values = append(reference.Values, ReferenceValue{AgentID: v.AgentID, Value: v.Value, ExperimentID: v.ExperimentID, ConstraintsMet: met[v.ExperimentID]})
		}
		out = append(out, reference)
	}
	return out, nil
}

// mergeRanOn sets where each standing's and each reference value's job ran, in one read of the
// metrics store.
func (s *PlatformExperimentsService) mergeRanOn(ctx context.Context, out *PlatformExperimentResults) error {
	var ranOn []**domain.RanOn
	var ids []string
	for _, metric := range out.Metrics {
		for i := range metric.Standings {
			ranOn, ids = append(ranOn, &metric.Standings[i].RanOn), append(ids, metric.Standings[i].ExperimentID)
		}
	}
	for _, reference := range out.References {
		for i := range reference.Values {
			ranOn, ids = append(ranOn, &reference.Values[i].RanOn), append(ids, reference.Values[i].ExperimentID)
		}
	}
	jobs := map[string]*domain.Experiment{}
	for _, id := range ids {
		if id == "" || jobs[id] != nil {
			continue
		}
		job, err := s.store.GetExperiment(ctx, id)
		if err != nil {
			return err
		}
		if job != nil {
			jobs[id] = job
		}
	}
	if len(jobs) == 0 {
		return nil
	}
	where, err := metricsdb.LatestRanOn(ctx, s.metricsDBURL, slices.Collect(maps.Values(jobs)), time.Now().UTC())
	if err != nil {
		return err
	}
	for i, id := range ids {
		*ranOn[i] = where[id]
	}
	return nil
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
	best, _, err := s.bestPerAgent(ctx, pe, primary)
	if err != nil {
		return nil, fmt.Errorf("results: %w", err)
	}
	standings, err := s.rank(ctx, pe, primary, best)
	if err != nil {
		return nil, err
	}
	out := make([]AgentResult, 0, len(standings))
	for _, st := range standings {
		out = append(out, AgentResult{AgentID: st.AgentID, FinalMetric: st.Best})
	}
	return out, nil
}

// bestPerAgent is every agent's best on one metric over its COMPLETED jobs that hold every
// constraint: the one figure results, standings and the top-3 at close read. An agent none of
// whose jobs holds the constraints has no best and so no standing.
func (s *PlatformExperimentsService) bestPerAgent(ctx context.Context, pe *domain.PlatformExperiment, metric domain.MetricDefinition) (map[string]metricsdb.AgentBest, []string, error) {
	completed, err := s.store.CompletedJobIDs(ctx, pe.ID)
	if err != nil {
		return nil, nil, err
	}
	return metricsdb.BestPerAgentOnMetric(ctx, s.metricsDBURL, pe, metric, completed)
}

// rank presents the competitors' bests on one metric as standings: attaches each one's job, sorts
// and assigns ranks. It is the ranking a run is judged by: results and the top-3 recorded at close
// both read it, from the bests the stage ladder cuts with, so none of them can diverge.
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
				st.CodeRef, st.Env, st.Objective = exp.CodeRef, exp.Job.Env, exp.OnObjective()
				if !st.Objective {
					st.HypothesisID = exp.HypothesisID
				}
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
