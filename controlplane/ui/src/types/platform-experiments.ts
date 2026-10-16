import type { Actor } from '@/types/actors'
import type { HostedCoordinator } from '@/types/agents'

export enum PlatformExperimentStatus {
  DRAFT = 'draft',
  OPEN = 'open',
  RUNNING = 'running',
  CLOSED = 'closed',
}

// One competing job's full metric history within a platform experiment — a leaderboard/
// competition dashboard plots one line per series.
interface MetricSeriesPoint {
  timestamp: string
  value: number
}

export interface AgentMetricSeries {
  agent_id: string
  experiment_id: string
  metric_name: string
  // Shared metric_basis for every point in this series — see MetricDataPoint.metric_basis. A
  // job that switches basis mid-run produces a second, distinct series rather than one series
  // mixing two scales.
  metric_basis?: string
  points: MetricSeriesPoint[]
}

export interface MetricDefinition {
  key: string
  direction: 'maximize' | 'minimize'
  description?: string
  // role decides what this metric is for: ranking (counts for cuts/standings, the default when
  // omitted), constraint (must satisfy bound or the job is excluded from standings), attribute
  // (shown, never ranked), reference (an outside system measured beside the standings).
  role?: 'ranking' | 'constraint' | 'attribute' | 'reference'
  bound?: number
  // On a ranking metric: the goal as a signed change against the baseline's value, 0.5 for +50%.
  target_vs_baseline?: number
  // The same code and data give the same value on any machine and in every run.
  deterministic?: boolean
}

// Why a platform experiment is in its status, and whose move it waits for: a person of its
// organisation (owner), its coordinator, or nobody (platform). A closed one waits for nobody and
// carries no waits_for.
export interface StatusReason {
  reason: string
  // What an agent acts on. A page shows summary, never this.
  message: string
  // One plain sentence for a person: what is happening, what it affects, who acts.
  summary: string
  // What the platform's operator checks: only in answers to staff.
  operator_detail?: string
  // When the condition began, where known.
  since?: string
  waits_for?: 'owner' | 'coordinator' | 'platform'
}

/** How many researchers of one agent type on one model an experiment keeps at work. */
export interface ResearcherPoolEntry {
  agent_type?: string
  model?: string
  hyperparameters?: Record<string, unknown>
  count: number
}

/** The researchers at work: in all, per entry of the pool in its order, and outside it. */
export interface ResearchersAtWork {
  total: number
  pool: { agent_type?: string; model?: string; at_work: number }[]
  outside_pool: number
}

export interface PlatformExperiment {
  id: string
  project_id: string
  name: string
  description?: string
  budget_accelerator_hours: number
  // How many researchers may be at work at once, its owner's to change.
  max_agents: number
  // Who it is to keep at work, at most max_agents in all. Where the platform runs them it replaces
  // one that finished or lost its machine, and a cut lowers the counts. Empty on a draft.
  researchers?: ResearcherPoolEntry[]
  // The researchers at work by the one rule max_agents holds to, answered with the experiment.
  researchers_at_work?: ResearchersAtWork
  // The places of researchers the platform keeps empty and why, absent when none.
  researchers_held?: string
  // Accelerators-in-flight (SUBMITTED+RUNNING) this experiment may hold at once. Absent uses the
  // platform default (quota.default_max_concurrent_accelerators).
  max_concurrent_accelerators?: number
  // While open: when its baseline began to be measured, created or accepted or a later time
  // stated. Once running: when the run started. The zero time on a draft.
  starts_at?: string
  // duration_hours is the run's length from its start, and ends_at is that start plus
  // duration_hours, absent until it runs. started_at is when the first researcher was at work. A
  // draft has neither.
  started_at?: string
  duration_hours?: number
  ends_at?: string
  // On a running one with no started_at: when the platform closes it if no researcher's machine
  // is running by then.
  agents_due_at?: string
  // On an open one: when the platform closes it if it is not running by then.
  baseline_due_at?: string
  status: PlatformExperimentStatus
  // On every read of one, in every status, and never in a list.
  status_reason?: StatusReason
  // What it has spent of its budget. On a read of one that is no draft.
  consumed_accelerator_hours?: number
  stages: Stage[]  // the elimination ladder
  current_stage: number               // 1-based index into stages
  signup_count: number
  signed_up_agents?: string[]
  metrics?: MetricDefinition[]
  report_interval_seconds?: number
  // The code it works on, owner/name on GitHub, and the address agents clone and jobs check out.
  // Both absent when it names none. Fixed at creation.
  repository?: string
  repository_url?: string
  // The image its hosted agents and coordinator run. Absent means the platform's own.
  agent_image?: string
  // Absent when the platform runs no coordinator for it.
  coordinator?: HostedCoordinator
  // The baseline no experiment starts without: the completed job that measured it, absent until
  // one is declared.
  baseline_job_id?: string
  // Each raise of the baseline while the run went on, oldest first. On a read of one experiment.
  baseline_raises?: BaselineRaise[]
  // The commit the baseline stands for, repository_url@sha.
  baseline_code_ref?: string
  // Staff shared it with every account: listed in every project, read by all, written by its own org alone.
  shared: boolean
  // Why a closed one closed, and whose credential closed it when a caller did. Absent on one
  // closed before reasons were kept.
  closed_at?: string
  close_reason?: 'end_time_reached' | 'closed_by_request' | 'planning_limit_reached' | 'proposal_expired' | 'baseline_limit_reached' | 'agents_not_started' | 'planner_stopped' | 'repository_not_created'
  closed_by?: Actor
  // The reason whoever closed it gave, absent when they gave none.
  close_message?: string
  // The coordinator's closing summary of the run. Absent until one is filed.
  summary?: string
  created_at: string
}

/** A re-declaration of a running experiment's baseline to a job that measured the control stronger. */
export interface BaselineRaise {
  from: string
  to: string
  reason: string
  detail: string
  actor: Actor
  at: string
}

export interface Stage {
  length_pct: number
  evict_pct: number
  /** Longest a single job may run while this stage is current. 0/absent = unlimited. */
  max_job_hours?: number
}

export interface AgentCut {
  agent_id: string
  stage_index: number
}

export interface StageAdvance {
  stage_index: number
  advanced_at: string
}

/** Deliberately carries no per-agent rank or standings. */
export interface StagesStatus {
  stages: Stage[]
  current_stage: number
  progress: number
  next_boundary_progress: number
  advances: StageAdvance[]
  active_agents: string[]
  cut_agents: AgentCut[]
}

export interface AgentStanding {
  rank: number
  agent_id: string
  best: number
  experiment_id?: string
  // The hypothesis that job tested and the env it ran with. The baseline's job tested none:
  // objective is true in place of hypothesis_id.
  hypothesis_id?: string
  objective?: boolean
  env?: Record<string, string>
}

// Standings rank the competitors on each ranking metric, the first being the primary one. bests
// holds every reporting agent's best raw value on every declared metric and the job that set it.
export interface PlatformExperimentResults {
  // baseline is the declared baseline job's last value on the metric, absent until it reported one.
  metrics: { metric: string; direction: 'maximize' | 'minimize'; baseline?: number; standings: AgentStanding[] }[]
  bests: {
    metric: string
    agents: { agent_id: string; best: number; experiment_id?: string }[]
    non_raw_basis_agents?: string[]
  }[]
  references?: MetricReference[]
}

// A value on a reference metric counts toward best only where its job met every constraint.
export interface MetricReference {
  metric: string
  direction: 'maximize' | 'minimize'
  values: { agent_id: string; value: number; experiment_id?: string; constraints_met: 'true' | 'false' | 'unknown' }[]
  best?: number
  leader_metric?: string
  leader?: number
  beaten?: boolean
}

/** What a repository's .hypothesisloop/experiment.yaml states of a platform experiment. */
export interface ExperimentFile {
  repository: string
  name?: string
  description?: string
  metrics?: MetricDefinition[]
  report_interval_seconds?: number
  stages?: Stage[]
}

/** Where a create with these repository fields would keep its code. */
export interface RepositoryCheck {
  repository?: string
  repository_url?: string
  /** The account a new repository would be created on. */
  repository_account?: string
}
