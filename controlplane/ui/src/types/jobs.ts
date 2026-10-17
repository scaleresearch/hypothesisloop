import type { Actor } from '@/types/actors'

// Exactly domain.ValidExperimentStatus's set (domain/constants.go).
export enum ExperimentStatus {
  SUBMITTED = 'SUBMITTED',
  QUEUED = 'QUEUED',
  RUNNING = 'RUNNING',
  COMPLETED = 'COMPLETED',
  FAILED = 'FAILED',
  EVICTED = 'EVICTED',
  REJECTED = 'REJECTED',
  CANCELLED = 'CANCELLED',
}

// AcceleratorType is an open, operator-defined identifier (see hypothesisloop.yaml's accelerator_types) — any
// vendor's model name is valid (NVIDIA, AMD, ...), not a fixed set. fetchResourceCapacity() has
// the types a cluster reports and their rates.
type AcceleratorType = string

type CapacityTier = 'guaranteed' | 'burst'

// Mirrors domain.MetricDataPoint (domain/metrics.go). Every field is always sent, so
// none of them are optional — marking them optional invited `?? somethingElse` fallbacks to
// fields that never existed.
export interface MetricDataPoint {
  experiment_id: string
  fraction_complete: number
  metric_name: string
  metric_value: number
  // What scale/definition metric_value is on. "raw" (the default) means unmodified; anything
  // else means the job transformed what the number means and it must not be compared to a
  // "raw" value from another run — show it next to the value, don't drop it.
  metric_basis: string
  recorded_at: string
}

// JobSpec mirrors the backend's domain.JobSpec — the platform's own execution DSL (never a
// raw k8s manifest). See controlplane/settings/examples/experiment-submission.yaml for the full reference.
interface JobSpec {
  image: string
  command?: string[]
  args?: string[]
  env?: Record<string, string>
  cpu?: string
  memory?: string
  storage?: string
  accelerator_type: AcceleratorType
  accelerator_count: number
  acceptable_accelerator_types?: AcceleratorType[]
  num_nodes?: number
  max_retries?: number
  topology?: { spread_across_hosts?: boolean; same_zone?: boolean }
  shm_size?: string
  // Any k8s extended resource beyond accelerator_type/accelerator_count — TPUs, other accelerators. Not
  // billed/capped (see domain.JobSpec.ExtraResources), passed straight through as pod
  // resource requests.
  extra_resources?: Record<string, string>
}

// Mirrors domain.PhaseDetail (domain/constants.go) — the runtime's explanation for a
// container that hasn't started or keeps restarting.
interface PhaseDetail {
  reason?: string
  message?: string
  restart_count?: number
}

// Mirrors domain.JobMetric (domain/metrics.go). value is the one figure the job is read
// and ranked by: the last value it reported for the metric.
export interface JobMetric {
  name: string
  value: number
  basis: string
}

// Mirrors domain.Experiment (domain/experiment.go). Fields the Go struct does not have
// do not belong here: an optional phantom field type-checks fine and renders as an em dash
// forever, which reads as "not reported yet" rather than "this was never real".
//
// Observed cost and final metric are deliberately absent — both live in the metrics store and are
// read from it (important.md #3), never off the job record.
export interface Experiment {
  id: string
  parent_id?: string
  agent_id: string
  /** What that agent is called now, its id when it has no name. */
  agent_name: string
  /** Who submitted it; agent_id is whose job it is. */
  actor: Actor
  platform_experiment_id: string
  project_id: string
  cluster_name?: string
  capacity_tier?: CapacityTier
  // The hypothesis the job tests. A baseline agent's job tests none: it has no hypothesis_id,
  // objective is true and hypothesis is empty.
  hypothesis_id?: string
  objective?: boolean
  hypothesis: string
  theory?: string
  status: ExperimentStatus
  estimated_cost_acch?: number
  // Set once this terminal job's final usage has been durably written to the metrics DB. Absent
  // means settlement is still pending; meaningless for a non-terminal job.
  quota_settled_at?: string
  accelerator_type: AcceleratorType
  accelerator_count: number
  estimated_duration_hours?: number
  novelty_score?: number
  priority_score: number
  // The job's own DSL — image, command, resources, accelerator count/type, distributed topology.
  job?: JobSpec
  created_at: string
  updated_at: string
  queued_at?: string
  // Set when the job was actually handed to a cluster — distinct from created_at, when the
  // registry row was first written.
  submitted_at?: string
  eviction_reason?: string
  // Why a QUEUED job waits: a code, and the scheduler's sentence where there is more to say.
  not_admitted_reason?: string
  not_admitted_message?: string
  // The runtime's latest explanation for why this job's container hasn't started or keeps
  // restarting, merged in live from the metrics store on every read.
  phase_detail?: PhaseDetail
  // The last value the job reported on its platform experiment's first ranking metric, read live
  // from the metrics store. Absent until the job reports that metric.
  metric?: JobMetric
  // The same for every declared metric the job reported, in declared order.
  metrics?: JobMetric[]
  // The write-up its agent filed, on a read of one job only.
  summary?: string
  // When the job reached its terminal status; absent while it can still run.
  finished_at?: string
  // Lineage: the commit it ran, repository_url@sha, and the branch that commit was pushed on, one
  // agent's implementation of the job's hypothesis. branch is empty on a job from before branches
  // were kept.
  code_ref: string
  branch: string
  config_hash?: string
  data_ref?: string
}

// Whole-set counts for the same filters fetchExperimentsPage accepts, counted server-side.
// Every list read is capped at one page, so a dashboard must count here rather than tally rows
// it fetched — a tally of a page is a tally of the page, not of the platform.
export interface ExperimentStats {
  total: number
  by_status: Record<string, number>
  by_capacity_tier: Record<string, number>
  running_by_capacity_tier: Record<string, number>
  evicted_by_capacity_tier: Record<string, number>
  evictions_by_reason: Record<string, number>
  // evictions_by_class folds evictions_by_reason into whose fault each was — the agent's own
  // ('workload'), the environment's ('infrastructure'), or the platform's own decision
  // ('policy'). Derived server-side from the same tally; see domain/fault_class.go.
  evictions_by_class: Record<string, number>
  // accelerator_count summed over SUBMITTED and RUNNING jobs: those on an accelerator type, and
  // those on CPU, whose count is cores.
  in_flight_accelerators: number
  in_flight_cores: number
  // Present only when group_by was asked for: the same stats per agent or hypothesis id.
  groups?: Record<string, ExperimentStats>
}
