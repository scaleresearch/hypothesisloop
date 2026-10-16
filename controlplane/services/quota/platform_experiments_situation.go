package quota

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
)

// Situation is where a platform experiment stands now, in one read: its clock, its objective,
// the capacity its jobs could use, its budget and what each agent has going. It is computed on
// every read, desired state from the relational store and observed state from the metrics store.
type Situation struct {
	Run         SituationRun          `json:"run"`
	Objective   []SituationMetric     `json:"objective" doc:"Each ranking metric, the first being the primary one."`
	Capacity    SituationCapacity     `json:"capacity"`
	Budget      SituationBudget       `json:"budget"`
	Agents      []SituationAgent      `json:"agents" doc:"Every agent but a draft's planner. An agent's token reads its own row only."`
	InFlight    []SituationHypothesis `json:"hypotheses_in_flight,omitempty" doc:"Each hypothesis with jobs queued or running or checked out within two hours, and who is on it: two agents on one is one idea worked twice. Absent on an agent's token."`
	Ideas       SituationIdeas        `json:"ideas_from_people"`
	Coordinator *SituationCoordinator `json:"coordinator,omitempty" doc:"The hosted coordinator, absent on an agent's token and where the platform hosts none."`
}

// SituationHypothesis is one hypothesis with jobs in flight or a checkout that claims it.
type SituationHypothesis struct {
	HypothesisID string   `json:"hypothesis_id"`
	Queued       int      `json:"queued"`
	Running      int      `json:"running"`
	Agents       []string `json:"agents" doc:"The names of the agents whose jobs these are or who checked it out within two hours."`
}

// SituationIdeas counts the open ideas of people: hypotheses a person registered that no agent
// has settled or built on yet. Their text is read with the command it names.
type SituationIdeas struct {
	Open int    `json:"open"`
	List string `json:"list" doc:"The hl command that lists them."`
}

// PersonIdeas counts the open ideas of people in a platform experiment. Implemented by
// graphdb.Store.
type PersonIdeas interface {
	OpenPersonIdeas(ctx context.Context, platformExperimentID string) (int, error)
}

// SituationRun is the run's clock.
type SituationRun struct {
	Now           time.Time          `json:"now"`
	Status        string             `json:"status"`
	ClosedAt      *time.Time         `json:"closed_at,omitempty" doc:"When the run closed. Present on a closed run alone."`
	CloseReason   domain.CloseReason `json:"close_reason,omitempty" doc:"Why the run closed, on a closed run."`
	NotStarted    bool               `json:"not_started,omitempty" doc:"True while the run has not started: a draft, or open while its baseline is measured. Its clock starts once the baseline is declared."`
	DurationHours *float64           `json:"duration_hours,omitempty" doc:"On a run not started: how long it runs once it starts."`
	EndsAt        *time.Time         `json:"ends_at,omitempty" doc:"When the run ends and the platform closes it. Absent until the run starts and once it closed."`
	MinutesLeft   *int               `json:"minutes_left,omitempty" doc:"Whole minutes until ends_at, 0 on a closed run. Absent on a run not started."`
	Stage         *SituationStage    `json:"stage,omitempty" doc:"The stage the run is in, while it runs."`
}

// clockOf is the clock of pe at now: a closed run has no time left whatever its ends_at, and one
// not started counts no minutes.
func clockOf(pe *domain.PlatformExperiment, now time.Time) SituationRun {
	out := SituationRun{Now: now, Status: string(pe.Status)}
	switch pe.Status {
	case domain.PlatformExpClosed:
		zero := 0
		out.ClosedAt, out.CloseReason, out.MinutesLeft = pe.ClosedAt, pe.CloseReason, &zero
	case domain.PlatformExpDraft, domain.PlatformExpOpen:
		out.NotStarted = true
		if pe.DurationHours > 0 {
			hours := pe.DurationHours
			out.DurationHours = &hours
		}
	default:
		out.MinutesLeft = minutesUntil(pe.EndsAt, now)
		if !pe.EndsAt.IsZero() {
			endsAt := pe.EndsAt.UTC()
			out.EndsAt = &endsAt
		}
	}
	return out
}

// SituationStage is the stage a running run is in.
type SituationStage struct {
	Index          int        `json:"index" doc:"1-based."`
	Of             int        `json:"of" doc:"How many stages the run has."`
	NextBoundaryAt *time.Time `json:"next_boundary_at,omitempty" doc:"When the next stage starts by the clock, earlier once the same part of the budget is spent. Absent in the last stage."`
}

// SituationMetric is where the run stands on one ranking metric.
type SituationMetric struct {
	Metric         string   `json:"metric"`
	Direction      string   `json:"direction"`
	Deterministic  bool     `json:"deterministic,omitempty" doc:"The metric is declared deterministic: its values compare across machines and need no repeat."`
	BaselineValue  *float64 `json:"baseline_value,omitempty" doc:"The declared baseline job's value."`
	BestValue      *float64 `json:"best_value,omitempty" doc:"The best value of a researcher's completed job that holds every constraint, as the standings rank it."`
	BestJob        string   `json:"best_job,omitempty"`
	BestAgent      string   `json:"best_agent,omitempty" doc:"The agent id of best_job."`
	BestCodeRef    string   `json:"best_code_ref,omitempty" doc:"The commit best_job ran, repository_url@sha: work meant to build on the best starts from it."`
	BestBranch     string   `json:"best_branch,omitempty" doc:"The branch best_job's commit was pushed on."`
	Target         *float64 `json:"target,omitempty" doc:"The value the metric's target_vs_baseline sets against baseline_value. Absent where the metric declares none or no baseline value is measured."`
	GapToTargetPct *float64 `json:"gap_to_target_pct,omitempty" doc:"How far best_value is short of target, in percent of target: positive while short of it, negative past it."`
	Best1hAgo      *float64 `json:"best_1h_ago,omitempty" doc:"best_value as it stood an hour ago, of the jobs that had completed by then."`
	Best3hAgo      *float64 `json:"best_3h_ago,omitempty" doc:"The same three hours ago."`
}

// SituationCapacity is the live capacity the run's jobs could use.
type SituationCapacity struct {
	Clusters        []SituationCluster `json:"clusters" doc:"For each cluster the run's jobs can be placed on, the type of its baseline job, or every type while none is declared: free and total now."`
	FitsNow         *int               `json:"fits_now,omitempty" doc:"How many jobs shaped like the run's baseline job, every node of it, the free capacity places now, node by node. Absent while no baseline job is declared."`
	JobsInFlightRun JobCounts          `json:"jobs_in_flight_run" doc:"The run's jobs queued and running, of every agent."`
}

// SituationCluster is one type's free and total capacity on one cluster.
type SituationCluster struct {
	Cluster         string          `json:"cluster"`
	AcceleratorType string          `json:"accelerator_type"`
	Kind            domain.TypeKind `json:"kind"`
	Free            int64           `json:"free"`
	Total           int64           `json:"total"`
}

// JobCounts counts jobs queued and running.
type JobCounts struct {
	Queued  int `json:"queued"`
	Running int `json:"running"`
}

// SituationBudget is the run's budget and spend.
type SituationBudget struct {
	RunBudgetAccH float64 `json:"run_budget_acch"`
	RunUsedAccH   float64 `json:"run_used_acch"`
}

// SituationAgent is what one agent has going.
type SituationAgent struct {
	AgentID                   string                   `json:"agent_id"`
	Name                      string                   `json:"name"`
	Role                      domain.SignupRole        `json:"role"`
	Ended                     domain.RunEnd            `json:"ended,omitempty" doc:"Why its run is over: closed, cut or finished. Absent while it goes on."`
	Jobs                      JobCounts                `json:"jobs"`
	Subagents                 *int                     `json:"subagents,omitempty"`
	Turn                      *domain.AgentTurn        `json:"turn,omitempty"`
	Activity                  *domain.AgentActivity    `json:"activity,omitempty"`
	MinutesWithoutJobInFlight *int                     `json:"minutes_without_job_in_flight,omitempty"`
	Messages                  *domain.MessageAllowance `json:"messages,omitempty"`
	Model                     *domain.ModelAllowance   `json:"model,omitempty"`
	Budget                    *AgentBudget             `json:"budget,omitempty" doc:"Absent for an agent that holds no quota."`
}

// AgentBudget is an agent's share and what is left of it, as GET /platform-experiments/{id}/quotas
// reads it.
type AgentBudget struct {
	Share          float64  `json:"share" doc:"share_accelerator_hours: all it may spend so far."`
	Used           float64  `json:"used" doc:"Spent, guaranteed and burst together."`
	RoomGuaranteed float64  `json:"room_guaranteed" doc:"The largest estimated_cost_acch a guaranteed job submitted now is admitted with."`
	RoomBurst      float64  `json:"room_burst" doc:"The same for a burst job."`
	NextShareAccH  *float64 `json:"next_share_acch,omitempty" doc:"What share becomes at the next stage, at the least. Present with run.stage.next_boundary_at."`
}

// SituationCoordinator is the hosted coordinator.
type SituationCoordinator struct {
	SummaryWrittenAt          *time.Time             `json:"summary_written_at,omitempty" doc:"When the summary was last written, absent while it never was."`
	JobsCompletedSinceSummary int                    `json:"jobs_completed_since_summary" doc:"How many jobs of the run completed after summary_written_at, every one while it was never written: findings the summary does not hold yet."`
	Model                     *domain.ModelAllowance `json:"model,omitempty"`
}

// Placement is the live capacity jobs of one org can be placed on: each cluster's lines, and
// what is free on each of its nodes now, cluster -> node -> key.
type Placement struct {
	Clusters         []ClusterCapacity
	NodeAccelerators map[string]map[string]map[string]int64
	NodeResources    map[string]map[string]map[string]int64
}

// placementOf reads the live capacity jobs of the org of projectID can be placed on.
func (h *PlatformExperimentsHandler) placementOf(ctx context.Context, projectID string) (*Placement, error) {
	orgID, err := h.clusters.ProjectOrg(ctx, projectID)
	if err != nil {
		return nil, err
	}
	clusters, err := h.liveCapacity(ctx, orgID, false)
	if err != nil {
		return nil, err
	}
	accelerators, err := metricsdb.LiveClusterNodeAcceleratorCapacity(ctx, h.metricsDBURL, h.capacityFreshness)
	if err != nil {
		return nil, err
	}
	resources, err := metricsdb.LiveClusterNodeResourceCapacity(ctx, h.metricsDBURL, h.capacityFreshness)
	if err != nil {
		return nil, err
	}
	out := &Placement{Clusters: clusters, NodeAccelerators: map[string]map[string]map[string]int64{}, NodeResources: map[string]map[string]map[string]int64{}}
	for _, cluster := range clusters {
		out.NodeAccelerators[cluster.ClusterName] = accelerators[cluster.ClusterName]
		out.NodeResources[cluster.ClusterName] = resources[cluster.ClusterName]
	}
	return out, nil
}

// maxFitsPerCluster bounds the jobs fitsNow counts on one cluster.
const maxFitsPerCluster = 1000

// fitsNow is how many jobs of shapes, on accelerator type t, the free capacity of p places now:
// each job on one cluster, each of its nodes on a node of that cluster with room for it, first
// fit, every job taking what it holds before the next is placed. Nil for a job that asks for
// nothing, which no count bounds.
func fitsNow(shapes []domain.NodeShape, t domain.AcceleratorType, p *Placement) *int {
	if !slices.ContainsFunc(shapes, func(shape domain.NodeShape) bool { return shape.AcceleratorCount > 0 || len(shape.Resources) > 0 }) {
		return nil
	}
	total := 0
	for _, cluster := range p.Clusters {
		accelerators, resources := map[string]int64{}, map[string]map[string]int64{}
		for node, byType := range p.NodeAccelerators[cluster.ClusterName] {
			accelerators[node] = 0
			for key, free := range byType {
				if domain.AcceleratorType(key).Is(t) {
					accelerators[node] += free
				}
			}
		}
		for node, free := range p.NodeResources[cluster.ClusterName] {
			resources[node] = maps.Clone(free)
			if _, listed := accelerators[node]; !listed {
				accelerators[node] = 0
			}
		}
		nodes := slices.Sorted(maps.Keys(accelerators))
		for placed := 0; placed < maxFitsPerCluster; placed++ {
			if !placeJob(shapes, nodes, accelerators, resources) {
				break
			}
			total++
		}
	}
	return &total
}

// placeJob places every node of a job on nodes, taking what each holds from accelerators and
// resources, or takes nothing and answers false when one node of it finds no room.
func placeJob(shapes []domain.NodeShape, nodes []string, accelerators map[string]int64, resources map[string]map[string]int64) bool {
	takenAccelerators, takenResources := map[string]int64{}, map[string]map[string]int64{}
	for _, shape := range shapes {
		placed := false
		for _, node := range nodes {
			if accelerators[node]-takenAccelerators[node] < shape.AcceleratorCount {
				continue
			}
			room := true
			for key, amount := range shape.Resources {
				if resources[node] == nil || resources[node][key]-takenResources[node][key] < amount {
					room = false
					break
				}
			}
			if !room {
				continue
			}
			takenAccelerators[node] += shape.AcceleratorCount
			if takenResources[node] == nil {
				takenResources[node] = map[string]int64{}
			}
			for key, amount := range shape.Resources {
				takenResources[node][key] += amount
			}
			placed = true
			break
		}
		if !placed {
			return false
		}
	}
	for node, amount := range takenAccelerators {
		accelerators[node] -= amount
	}
	for node, taken := range takenResources {
		for key, amount := range taken {
			resources[node][key] -= amount
		}
	}
	return true
}

// runCapacity is the capacity block of pe: the lines of its baseline job's type, or of every type
// while none is declared, on the clusters its jobs can be placed on, and how many jobs shaped
// like its baseline job place now.
func (s *PlatformExperimentsService) runCapacity(ctx context.Context, pe *domain.PlatformExperiment) (SituationCapacity, error) {
	out := SituationCapacity{Clusters: []SituationCluster{}}
	p, err := s.placement(ctx, pe.ProjectID)
	if err != nil {
		return out, fmt.Errorf("capacity: %w", err)
	}
	var baseline *domain.Experiment
	if pe.BaselineJobID != "" {
		if baseline, err = s.store.GetExperiment(ctx, pe.BaselineJobID); err != nil {
			return out, fmt.Errorf("capacity: baseline job: %w", err)
		}
		if baseline == nil {
			return out, fmt.Errorf("capacity: baseline job %s of %s does not exist", pe.BaselineJobID, pe.ID)
		}
	}
	for _, cluster := range p.Clusters {
		for _, line := range cluster.Accelerators {
			if baseline != nil && !domain.AcceleratorType(line.AcceleratorType).Is(baseline.AcceleratorType) {
				continue
			}
			out.Clusters = append(out.Clusters, SituationCluster{Cluster: cluster.ClusterName, AcceleratorType: line.AcceleratorType, Kind: line.Kind, Free: line.Available, Total: line.Total})
		}
	}
	if baseline != nil {
		out.FitsNow = fitsNow(baseline.NodeShapes(), baseline.AcceleratorType, p)
	}
	return out, nil
}

// standing is one job's value on a metric, its agent and when it completed.
type standing struct {
	agentID, jobID string
	value          float64
	completedAt    time.Time
}

// metricStanding is where the researchers stand on metric at now, from the values of their
// rankable completed jobs: the best, and the best of the jobs completed an hour and three hours
// before.
func metricStanding(metric domain.MetricDefinition, values []standing, now time.Time) SituationMetric {
	out := SituationMetric{Metric: metric.Key, Direction: metric.Direction, Deterministic: metric.Deterministic}
	better := func(best *float64, value float64) bool { return best == nil || metric.Better(value, *best) }
	for _, v := range values {
		if better(out.BestValue, v.value) {
			value := v.value
			out.BestValue, out.BestJob, out.BestAgent = &value, v.jobID, v.agentID
		}
		if !v.completedAt.After(now.Add(-time.Hour)) && better(out.Best1hAgo, v.value) {
			value := v.value
			out.Best1hAgo = &value
		}
		if !v.completedAt.After(now.Add(-3*time.Hour)) && better(out.Best3hAgo, v.value) {
			value := v.value
			out.Best3hAgo = &value
		}
	}
	return out
}

// gapToTarget is how far best is short of target in percent of target, positive while short of
// it: nil without both, or with a target of 0, which no percentage measures.
func gapToTarget(direction string, best, target *float64) *float64 {
	if best == nil || target == nil || *target == 0 {
		return nil
	}
	gap := (*target - *best) / math.Abs(*target) * 100
	if direction == "minimize" {
		gap = -gap
	}
	gap = math.Round(gap*10) / 10
	return &gap
}

// objective is where the run stands on each ranking metric of pe at now.
func (s *PlatformExperimentsService) objective(ctx context.Context, pe *domain.PlatformExperiment, now time.Time) ([]SituationMetric, error) {
	out := []SituationMetric{}
	completed, err := s.store.CompletedJobs(ctx, pe.ID)
	if err != nil {
		return nil, fmt.Errorf("objective: %w", err)
	}
	ids, completedAt, byID := map[string]bool{}, map[string]time.Time{}, map[string]*domain.Experiment{}
	for _, job := range completed {
		ids[job.ID] = true
		byID[job.ID] = job
		if job.FinishedAt != nil {
			completedAt[job.ID] = *job.FinishedAt
		} else {
			completedAt[job.ID] = job.UpdatedAt
		}
	}
	rankable, err := metricsdb.RankableJobs(ctx, s.metricsDBURL, pe, ids)
	if err != nil {
		return nil, fmt.Errorf("objective: %w", err)
	}
	competitors, err := s.store.ListSignupsByRole(ctx, pe.ID, domain.SignupRoleCompetitor)
	if err != nil {
		return nil, fmt.Errorf("objective: %w", err)
	}
	for _, metric := range pe.Metrics {
		if metric.EffectiveRole() != domain.MetricRoleRanking {
			continue
		}
		values, err := metricsdb.LastPerJobOnMetric(ctx, s.metricsDBURL, pe, metric, rankable)
		if err != nil {
			return nil, fmt.Errorf("objective: %w", err)
		}
		var standings []standing
		for _, v := range values {
			if slices.Contains(competitors, v.AgentID) {
				standings = append(standings, standing{agentID: v.AgentID, jobID: v.ExperimentID, value: v.Value, completedAt: completedAt[v.ExperimentID]})
			}
		}
		m := metricStanding(metric, standings, now)
		if best := byID[m.BestJob]; best != nil {
			m.BestCodeRef, m.BestBranch = best.CodeRef, best.Branch
		}
		if pe.BaselineJobID != "" {
			baseline, err := metricsdb.LastPerJobOnMetric(ctx, s.metricsDBURL, pe, metric, map[string]bool{pe.BaselineJobID: true})
			if err != nil {
				return nil, fmt.Errorf("objective: %w", err)
			}
			if len(baseline) > 0 {
				m.BaselineValue = &baseline[0].Value
			}
		}
		if m.BaselineValue != nil {
			m.Target = metric.Target(*m.BaselineValue)
			m.GapToTargetPct = gapToTarget(metric.Direction, m.BestValue, m.Target)
		}
		out = append(out, m)
	}
	return out, nil
}

// minutesUntil is the whole minutes from now to at, 0 once it passed, nil for no time.
func minutesUntil(at, now time.Time) *int {
	if at.IsZero() {
		return nil
	}
	minutes := max(0, int(at.Sub(now).Minutes()))
	return &minutes
}

// Situation is where platform experiment id stands now, with the messages reader may still send each agent. With agentID set, the answer of that
// agent's token, it holds that agent's row alone and no coordinator.
func (s *PlatformExperimentsService) Situation(ctx context.Context, id, agentID string, reader domain.Actor) (*Situation, error) {
	pe, err := s.store.GetPlatformExperiment(ctx, id)
	if err != nil {
		return nil, err
	}
	if pe == nil {
		return nil, ErrPlatformExperimentNotFound
	}
	now := time.Now().UTC()
	out := &Situation{Run: clockOf(pe, now)}
	if pe.Status == domain.PlatformExpRunning {
		out.Run.Stage = &SituationStage{Index: pe.CurrentStage, Of: len(pe.Stages)}
		next, err := s.nextStage(ctx, pe)
		if err != nil {
			return nil, fmt.Errorf("situation: %w", err)
		}
		if next != nil {
			out.Run.Stage.NextBoundaryAt = &next.startsAt
		}
	}
	if out.Objective, err = s.objective(ctx, pe, now); err != nil {
		return nil, fmt.Errorf("situation: %w", err)
	}
	if out.Capacity, err = s.runCapacity(ctx, pe); err != nil {
		return nil, fmt.Errorf("situation: %w", err)
	}
	if err := s.Spend(ctx, pe); err != nil {
		return nil, fmt.Errorf("situation: %w", err)
	}
	out.Budget = SituationBudget{RunBudgetAccH: pe.BudgetAcceleratorHours}
	if pe.ConsumedAcceleratorHours != nil {
		out.Budget.RunUsedAccH = domain.RoundHours(*pe.ConsumedAcceleratorHours)
	}
	signups, err := s.store.ListAgentSignups(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("situation: %w", err)
	}
	rows, err := s.roster(ctx, id, signups, reader)
	if err != nil {
		return nil, fmt.Errorf("situation: %w", err)
	}
	quotas, err := s.ListQuotas(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("situation: %w", err)
	}
	budgets := map[string]*AgentBudget{}
	for _, q := range quotas {
		budgets[q.AgentID] = &AgentBudget{Share: q.ShareAcceleratorHours, Used: domain.RoundHours(q.UsedGuaranteedAccH + q.UsedBurstAccH),
			RoomGuaranteed: q.RemainingGuaranteed, RoomBurst: q.RemainingBurst, NextShareAccH: q.NextStageShare}
	}
	out.Capacity.JobsInFlightRun = runJobsInFlight(rows)
	out.Agents = []SituationAgent{}
	for _, row := range rows {
		if row.Role == domain.SignupRolePlanner || (agentID != "" && row.AgentID != agentID) {
			continue
		}
		out.Agents = append(out.Agents, situationAgent(row, budgets[row.AgentID]))
	}
	if agentID == "" {
		if out.InFlight, err = s.hypothesesInFlight(ctx, id, rows); err != nil {
			return nil, fmt.Errorf("situation: %w", err)
		}
	}
	if s.personIdeas == nil {
		return nil, errors.New("situation: the hypothesis graph is not wired (WithPersonIdeas)")
	}
	out.Ideas = SituationIdeas{List: "hl hypothesis list --by people"}
	if out.Ideas.Open, err = s.personIdeas.OpenPersonIdeas(ctx, id); err != nil {
		return nil, fmt.Errorf("situation: %w", err)
	}
	if agentID == "" && pe.CoordinatorHosted && pe.Coordinator != nil {
		if out.Coordinator, err = s.situationCoordinator(ctx, pe); err != nil {
			return nil, fmt.Errorf("situation: %w", err)
		}
	}
	return out, nil
}

// runJobsInFlight counts the run's jobs queued and running, of every agent of rows.
func runJobsInFlight(rows []JoinedAgent) JobCounts {
	var out JobCounts
	for _, row := range rows {
		if row.InFlight != nil {
			out.Queued += row.InFlight.Queued
			out.Running += row.InFlight.Running
		}
	}
	return out
}

// hypothesesInFlight is each hypothesis of platform experiment id with jobs in flight or claimed by
// a checkout, its agents named as rows name them.
func (s *PlatformExperimentsService) hypothesesInFlight(ctx context.Context, id string, rows []JoinedAgent) ([]SituationHypothesis, error) {
	inFlight, err := s.store.HypothesesInFlight(ctx, id)
	if err != nil {
		return nil, err
	}
	names := map[string]string{}
	for _, row := range rows {
		names[row.AgentID] = row.Name
	}
	out := make([]SituationHypothesis, len(inFlight))
	for i, h := range inFlight {
		out[i] = SituationHypothesis{HypothesisID: h.HypothesisID, Queued: h.Queued, Running: h.Running, Agents: make([]string, len(h.AgentIDs))}
		for j, agent := range h.AgentIDs {
			out[i].Agents[j] = cmp.Or(names[agent], agent)
		}
	}
	return out, nil
}

func situationAgent(row JoinedAgent, budget *AgentBudget) SituationAgent {
	out := SituationAgent{AgentID: row.AgentID, Name: row.Name, Role: row.Role, Ended: row.Ended, Subagents: row.Subagents, Turn: row.Turn,
		Activity: row.Activity, MinutesWithoutJobInFlight: row.MinutesWithoutJobInFlight, Messages: row.Messages, Model: row.ModelAllowance, Budget: budget}
	if row.InFlight != nil {
		out.Jobs = JobCounts{Queued: row.InFlight.Queued, Running: row.InFlight.Running}
	}
	return out
}

// situationCoordinator is the hosted coordinator of pe as the situation answers it.
func (s *PlatformExperimentsService) situationCoordinator(ctx context.Context, pe *domain.PlatformExperiment) (*SituationCoordinator, error) {
	out := &SituationCoordinator{}
	var err error
	if out.SummaryWrittenAt, err = s.store.SummaryWrittenAt(ctx, pe.ID); err != nil {
		return nil, err
	}
	completed, err := s.store.CompletedJobs(ctx, pe.ID)
	if err != nil {
		return nil, err
	}
	for _, job := range completed {
		finished := job.UpdatedAt
		if job.FinishedAt != nil {
			finished = *job.FinishedAt
		}
		if out.SummaryWrittenAt == nil || finished.After(*out.SummaryWrittenAt) {
			out.JobsCompletedSinceSummary++
		}
	}
	states, err := metricsdb.LatestHarnessState(ctx, s.metricsDBURL, pe.ID, pe.CreatedAt)
	if err != nil {
		return nil, err
	}
	for _, state := range states {
		if state.Role == domain.CoordinatorHealthRole && state.AgentID == "" && liveReading(&pe.Coordinator.Instance, state) {
			out.Model = domain.ModelAllowanceOf(state.Model, time.Now())
		}
	}
	return out, nil
}

// liveReading says state was read from the machine instance holds now: one asked for under its
// name and not released, read since it was asked for. A reading of a machine that ended says
// nothing of the agent now.
func liveReading(instance *domain.AgentInstance, state domain.HarnessState) bool {
	return instance != nil && instance.Name != "" && !instance.Released && instance.AskedAt != nil && !state.At.Before(*instance.AskedAt)
}

// withWork sets on each row of pe what its agent has going: the subagents and turn its harness
// last reported and the model allowance it last read, of the machine it holds now, the messages
// reader may still send it, and how long it has had no job in flight.
func (s *PlatformExperimentsService) withWork(ctx context.Context, pe *domain.PlatformExperiment, rows []JoinedAgent, now time.Time, reader domain.Actor) error {
	states, err := metricsdb.LatestHarnessState(ctx, s.metricsDBURL, pe.ID, pe.CreatedAt)
	if err != nil {
		return err
	}
	allowance, err := s.store.MessageAllowance(ctx, pe.ID, reader)
	if err != nil {
		return err
	}
	byAgent := map[string]domain.HarnessState{}
	for _, state := range states {
		if state.AgentID != "" {
			byAgent[state.AgentID] = state
		}
	}
	for i := range rows {
		row := &rows[i]
		if row.Ended != "" {
			continue
		}
		if a, ok := allowance[row.AgentID]; ok {
			row.Messages = &a
		}
		if row.InFlight != nil {
			row.MinutesWithoutJobInFlight = row.InFlight.MinutesWithoutJob(now)
		}
		if state, ok := byAgent[row.AgentID]; ok && liveReading(row.Instance, state) {
			subagents := state.Subagents
			row.Subagents, row.Turn, row.ModelAllowance = &subagents, domain.TurnOf(state), domain.ModelAllowanceOf(state.Model, now)
			if state.InstructionsDigest != "" {
				if row.RunningPinnedInstructions, err = s.runsPinnedInstructions(ctx, pe.ID, row.AgentID, state.InstructionsDigest); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// runsPinnedInstructions says whether digest, the start of the digest of the instructions a
// harness runs, is that of the instructions agentID is served now, nil for an agent with none.
func (s *PlatformExperimentsService) runsPinnedInstructions(ctx context.Context, platformExpID, agentID, digest string) (*bool, error) {
	src, err := s.store.AgentInstructions(ctx, platformExpID, agentID)
	if err != nil || src == nil {
		return nil, err
	}
	pinned, err := composeInstructions(platformExpID, agentID, src)
	if err != nil {
		return nil, err
	}
	same := strings.HasPrefix(pinned.Digest, digest)
	return &same, nil
}

// registerSituationHuma registers the situation read.
func registerSituationHuma(doc *apidocs.Doc, h *PlatformExperimentsHandler) {
	access := doc.Access
	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "get-platform-experiment-situation", Method: "GET", Path: "/platform-experiments/{id}/situation",
		Summary: "Where the run stands now, in one read", Tags: []string{"platform-experiments"},
		Description: "Computed on every read: the run's clock (ends_at, minutes_left, the stage and when the next starts; on a closed run minutes_left 0 with closed_at and close_reason, on one not started how long it will run once it starts), " +
			"the objective on each ranking metric (the declared baseline's value, the best a researcher's completed job holding every constraint reached, by which job and agent, " +
			"the target its target_vs_baseline sets against the baseline's value and the gap to it, and the best an hour and three hours ago), " +
			"the capacity the run's jobs could use (free and total of its baseline job's type on each cluster its jobs can be placed on, fits_now: how many jobs shaped like the baseline job place now, node by node, and the run's jobs queued and running), " +
			"the run's budget and spend, for a coordinator, a person or a key each hypothesis with jobs queued or running or checked out within two hours and who is on it, and for each agent its jobs queued and running, the subagents and turn its harness last reported, its activity, minutes without a job in flight, " +
			"the messages the reader may still send it this hour, its model account's allowance, and its share and the room left in it, " +
			"and ideas_from_people: how many open ideas people registered, those no agent has settled or built on yet, and the command that lists them. " +
			"An agent's token reads the run and its own row. A coordinator, a person or a key reads every row and the hosted coordinator: when its summary was last written, how many jobs completed since, and its model allowance.",
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*struct{ Body *Situation }, error) {
		if err := access.PlatformExperiment(ctx, in.ID); err != nil {
			return nil, err
		}
		caller, err := h.caller(ctx)
		if err != nil {
			return nil, err
		}
		agentID := ""
		if caller.Kind == auth.KindAgent {
			agentID = caller.AgentID
		}
		situation, err := h.svc.Situation(ctx, in.ID, agentID, caller.Actor())
		if errors.Is(err, ErrPlatformExperimentNotFound) {
			return nil, huma.Error404NotFound(err.Error())
		}
		if err != nil {
			return nil, auth.Internal(h.logger, "get platform experiment situation", err, zap.String("id", in.ID))
		}
		return &struct{ Body *Situation }{Body: situation}, nil
	})
}
