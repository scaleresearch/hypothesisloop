// ---------------------------------------------------------------------------
// Enumerations
// ---------------------------------------------------------------------------

// Exactly domain.ValidExperimentStatus's set (shared/domain/constants.go).
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

enum CapacityTier {
  GUARANTEED = 'guaranteed',
  BURST = 'burst',
}

export enum PlatformExperimentStatus {
  DRAFT = 'draft',
  OPEN = 'open',
  RUNNING = 'running',
  CLOSED = 'closed',
}

// ---------------------------------------------------------------------------
// Metric contract
// ---------------------------------------------------------------------------


// ---------------------------------------------------------------------------
// Metric data point (timeseries emitted by running experiment)
// ---------------------------------------------------------------------------

// Mirrors domain.MetricDataPoint (shared/domain/metrics.go). Every field is always sent, so
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

// ---------------------------------------------------------------------------
// Experiment
// ---------------------------------------------------------------------------

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

// Mirrors domain.PhaseDetail (shared/domain/constants.go) — the runtime's explanation for a
// container that hasn't started or keeps restarting.
interface PhaseDetail {
  reason?: string
  message?: string
  restart_count?: number
}

// Mirrors domain.JobMetric (shared/domain/metrics.go). value is the one figure the job is read
// by: its best in the metric's direction on a ranking metric, its last on any other.
export interface JobMetric {
  name: string
  value: number
  basis: string
}

// Mirrors domain.Experiment (shared/domain/experiment.go). Fields the Go struct does not have
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
  hypothesis_id: string
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
  not_admitted_reason?: string
  // The runtime's latest explanation for why this job's container hasn't started or keeps
  // restarting, merged in live from the metrics store on every read.
  phase_detail?: PhaseDetail
  // The job's best and last value on its platform experiment's first ranking metric, read live
  // from the metrics store. Absent until the job reports that metric.
  metric?: JobMetric
  // The same for every declared metric the job reported, in declared order.
  metrics?: JobMetric[]
  // The write-up its agent filed, on a read of one job only.
  summary?: string
  /** The job completed and its agent filed no summary of it. */
  summary_due: boolean
  // When the job reached its terminal status; absent while it can still run.
  finished_at?: string
  // Lineage
  code_ref?: string
  config_hash?: string
  data_ref?: string
}

export interface MetricDefinition {
  key: string
  direction: 'maximize' | 'minimize'
  description?: string
  // role decides what this metric is for: ranking (counts for cuts/standings, the default when
  // omitted), constraint (must satisfy bound or the job is excluded from standings), attribute
  // (shown, never ranked). See docs/stages.md.
  role?: 'ranking' | 'constraint' | 'attribute'
  bound?: number
}

export interface PlatformExperiment {
  id: string
  project_id: string
  name: string
  description?: string
  budget_accelerator_hours: number
  max_agents: number
  // Accelerators-in-flight (SUBMITTED+RUNNING) this experiment may hold at once. Absent uses the
  // platform default (quota.default_max_concurrent_accelerators).
  max_concurrent_accelerators?: number
  // The earliest it may start: the zero time when none is stated.
  starts_at?: string
  // The run's own clock. started_at and ends_at are absent until the platform starts the run,
  // and ends_at is then started_at plus duration_hours. A draft has no duration_hours.
  started_at?: string
  duration_hours?: number
  ends_at?: string
  // On an open one: when the platform closes it if it is not running by then.
  baseline_due_at?: string
  status: PlatformExperimentStatus
  // On a draft or an open one, read by itself: where it stands, in words for a person.
  status_reason?: { reason: string; message: string }
  // What it has spent of its budget. On a read of one that is no draft.
  consumed_accelerator_hours?: number
  stages: Array<{ length_pct: number; evict_pct: number; max_job_hours?: number }>  // the elimination ladder
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
  // The completed job that measured the baseline. Absent until one is declared, and no
  // experiment starts without it.
  baseline_job_id?: string
  // Staff shared it with every account: listed in every project, read by all, written by its own org alone.
  shared: boolean
  // Why a closed one closed, and whose credential closed it when a caller did. Absent on one
  // closed before reasons were kept.
  closed_at?: string
  close_reason?: 'end_time_reached' | 'closed_by_request' | 'planning_limit_reached' | 'proposal_expired' | 'baseline_limit_reached'
  closed_by?: Actor
  created_at: string
}

// 50 common ML metrics for quick selection
export const COMMON_ML_METRICS: Array<{ key: string; label: string; direction: 'maximize' | 'minimize'; description: string }> = [
  { key: 'val_accuracy',        label: 'Val Accuracy',           direction: 'maximize', description: 'Fraction of validation examples classified correctly' },
  { key: 'val_loss',            label: 'Val Loss',               direction: 'minimize', description: 'Cross-entropy or task loss on the validation split' },
  { key: 'train_accuracy',      label: 'Train Accuracy',         direction: 'maximize', description: 'Training-set classification accuracy' },
  { key: 'train_loss',          label: 'Train Loss',             direction: 'minimize', description: 'Training loss (cross-entropy)' },
  { key: 'test_accuracy',       label: 'Test Accuracy',          direction: 'maximize', description: 'Held-out test-set accuracy' },
  { key: 'f1_score',            label: 'F1 Score',               direction: 'maximize', description: 'Harmonic mean of precision and recall (macro avg)' },
  { key: 'precision',           label: 'Precision',              direction: 'maximize', description: 'True positives / (true positives + false positives)' },
  { key: 'recall',              label: 'Recall',                 direction: 'maximize', description: 'True positives / (true positives + false negatives)' },
  { key: 'auc_roc',             label: 'AUC-ROC',                direction: 'maximize', description: 'Area under the receiver-operating curve' },
  { key: 'auc_pr',              label: 'AUC-PR',                 direction: 'maximize', description: 'Area under the precision-recall curve' },
  { key: 'top5_accuracy',       label: 'Top-5 Accuracy',         direction: 'maximize', description: 'Whether correct label is in model\'s top-5 predictions' },
  { key: 'bleu',                label: 'BLEU',                   direction: 'maximize', description: 'Bilingual Evaluation Understudy for text generation' },
  { key: 'rouge_l',             label: 'ROUGE-L',                direction: 'maximize', description: 'Longest-common-subsequence recall for summarization' },
  { key: 'perplexity',          label: 'Perplexity',             direction: 'minimize', description: 'Exponentiated average negative log-likelihood (language models)' },
  { key: 'bits_per_byte',       label: 'Bits-per-byte',          direction: 'minimize', description: 'Compression quality metric for language models' },
  { key: 'mse',                 label: 'MSE',                    direction: 'minimize', description: 'Mean squared error on the validation set' },
  { key: 'mae',                 label: 'MAE',                    direction: 'minimize', description: 'Mean absolute error on the validation set' },
  { key: 'rmse',                label: 'RMSE',                   direction: 'minimize', description: 'Root mean squared error' },
  { key: 'r2',                  label: 'R²',                     direction: 'maximize', description: 'Coefficient of determination (regression quality)' },
  { key: 'mape',                label: 'MAPE',                   direction: 'minimize', description: 'Mean absolute percentage error' },
  { key: 'mean_iou',            label: 'Mean IoU',               direction: 'maximize', description: 'Average intersection-over-union for semantic segmentation' },
  { key: 'dice_score',          label: 'Dice Score',             direction: 'maximize', description: 'Overlap metric widely used in medical image segmentation' },
  { key: 'pixel_accuracy',      label: 'Pixel Accuracy',         direction: 'maximize', description: 'Fraction of correctly classified pixels' },
  { key: 'ap50',                label: 'AP@50',                  direction: 'maximize', description: 'Average precision at IoU threshold 0.5 (object detection)' },
  { key: 'map',                 label: 'mAP',                    direction: 'maximize', description: 'Mean average precision across IoU thresholds' },
  { key: 'reward',              label: 'Reward',                 direction: 'maximize', description: 'Cumulative episode reward (reinforcement learning)' },
  { key: 'episode_length',      label: 'Episode Length',         direction: 'maximize', description: 'Steps survived per episode (RL survival tasks)' },
  { key: 'win_rate',            label: 'Win Rate',               direction: 'maximize', description: 'Fraction of games won against baseline opponent' },
  { key: 'elo',                 label: 'Elo Rating',             direction: 'maximize', description: 'Elo rating from self-play tournaments' },
  { key: 'human_eval_pass1',    label: 'HumanEval pass@1',       direction: 'maximize', description: 'Probability of passing unit tests on first attempt' },
  { key: 'gsm8k',               label: 'GSM8K Accuracy',         direction: 'maximize', description: 'Grade-school math reasoning benchmark' },
  { key: 'mmlu',                label: 'MMLU Score',             direction: 'maximize', description: 'Massive multitask language understanding accuracy' },
  { key: 'hellaswag',           label: 'HellaSwag',              direction: 'maximize', description: 'Common-sense NLI benchmark' },
  { key: 'throughput_samples',  label: 'Throughput (samples/s)', direction: 'maximize', description: 'Training throughput in samples per second' },
  { key: 'accelerator_utilization',     label: 'Accelerator Utilization %',      direction: 'maximize', description: 'Average accelerator kernel utilization during training' },
  { key: 'memory_gb',           label: 'Peak Memory (GB)',       direction: 'minimize', description: 'Peak accelerator memory consumed during a forward pass' },
  { key: 'latency_ms',          label: 'Latency (ms)',           direction: 'minimize', description: 'Inference latency per sample in milliseconds' },
  { key: 'flops',               label: 'FLOPs',                  direction: 'minimize', description: 'Floating-point operations required per inference' },
  { key: 'params_millions',     label: 'Params (M)',             direction: 'minimize', description: 'Total trainable parameters in millions' },
  { key: 'gradient_norm',       label: 'Gradient Norm',          direction: 'minimize', description: 'L2 norm of the parameter gradient (training stability)' },
  { key: 'learning_rate',       label: 'Learning Rate',          direction: 'minimize', description: 'Current learning rate (schedule tracking)' },
  { key: 'calibration_ece',     label: 'ECE',                    direction: 'minimize', description: 'Expected calibration error — how well confidence matches accuracy' },
  { key: 'ndcg',                label: 'NDCG@10',                direction: 'maximize', description: 'Normalised discounted cumulative gain at rank 10 (ranking tasks)' },
  { key: 'mrr',                 label: 'MRR',                    direction: 'maximize', description: 'Mean reciprocal rank for retrieval tasks' },
  { key: 'hit_rate_at_k',       label: 'Hit Rate @K',            direction: 'maximize', description: 'Fraction of queries where correct item appears in top-K' },
  { key: 'code_coverage',       label: 'Code Coverage %',        direction: 'maximize', description: 'Fraction of lines/branches covered by generated tests' },
  { key: 'security_score',      label: 'Security Score',         direction: 'maximize', description: 'Composite score from static-analysis security checks' },
  { key: 'compression_ratio',   label: 'Compression Ratio',      direction: 'maximize', description: 'Ratio of uncompressed to compressed size' },
  { key: 'downstream_accuracy', label: 'Downstream Accuracy',    direction: 'maximize', description: 'Accuracy on downstream task after fine-tuning or probing' },
  { key: 'custom',              label: 'Custom…',                direction: 'maximize', description: 'Define your own metric key and description' },
]

// Mirrors quota.AgentQuotaView. What remains is the largest estimate a job submitted now is
// admitted with, as admission counts it.
export interface AgentQuota {
  id: string
  agent_id: string
  platform_experiment_id: string
  share_accelerator_hours: number
  remaining_guaranteed_accelerator_hours: number
  remaining_burst_accelerator_hours: number
  guaranteed_accelerator_hours: number
  burst_accelerator_hours: number
  used_guaranteed_acch: number
  used_burst_acch: number
}

// ---------------------------------------------------------------------------
// Agent
// ---------------------------------------------------------------------------

// Mirrors domain.Agent (shared/domain/agent.go).
export interface Agent {
  id: string
  name: string
  performance_score: number
  top3_count: number       // number of top-3 placements ever (drives +25% bonus)
  created_at: string
}

// A registered target cluster and whether its cluster-agent is currently connected.
// Connectivity is a heartbeat the agent leaves on every desired-state poll (~2s) — the
// control plane never dials the cluster itself, so "connected" here means "we've heard
// from it recently," not "we can reach it."
export interface ClusterInfo {
  cluster_name: string
  // Stable fingerprint the runtime derives live (kube-system namespace UID / machine-id) —
  // unlike cluster_name, survives a rename. Empty for a cluster whose agent build predates this
  // or that hasn't reported within the freshness window. Everything scheduler-side that must
  // survive a rename (tried_clusters) keys on this, never on cluster_name.
  cluster_id: string
  last_seen_at: string
  connected: boolean
  // Actual busy-vs-idle chip counts from the cluster's most recent reconcile snapshot, summed
  // across every accelerator flavor — real hardware occupancy, not the used/budget AccH ratio
  // shown per platform experiment. Both are 0 when no live snapshot is within the freshness
  // window (e.g. a disconnected cluster).
  accelerator_busy: number
  accelerator_total: number
  // The same for the cores the cluster offers to jobs on CPU, never added to the chips.
  core_busy: number
  core_total: number
  // Whether this cluster sits behind a native autoscaler (cluster-autoscaler / Karpenter),
  // operator-set via AUTOSCALER_ENABLED on the agent deployment. Fail-closed: false means the
  // scheduler never speculatively submits onto this cluster.
  autoscaler_enabled: boolean
}

export interface ClustersResponse {
  clusters: ClusterInfo[]
}

// What the org that registered a cluster sets on it in place of the scheduler's defaults. A field
// that is absent uses the default.
export interface ClusterSettings {
  // Seconds a job placed ahead of capacity waits for the cluster's autoscaler, under 1800.
  scale_up_timeout_seconds?: number
  // The most accelerators that jobs placed ahead of capacity hold on the cluster at once.
  max_speculative_accelerators?: number
  // Seconds a job admitted to the cluster may stay unplaced before it is evicted.
  stuck_pending_timeout_seconds?: number
}

// ---------------------------------------------------------------------------
// Hypotheses
// ---------------------------------------------------------------------------

// The verdict on a claim: see POST /hypotheses/{id}/status. On an agent's hypothesis only that
// agent, or a person or key acting for it, may change it.
export type HypothesisStatus = 'open' | 'confirmed' | 'refuted' | 'inconclusive'

// Whose credential made a record, as the platform authenticated it. Nothing a request says sets
// it. `unverified` is a record from before actors were kept: its name is the one stated then.
export type ActorKind = 'user' | 'key' | 'agent' | 'coordinator' | 'staff' | 'unverified'

export interface Actor {
  kind: ActorKind
  /** The user, key, agent or platform experiment (for a coordinator); empty for staff and unverified. */
  id: string
  /** The name it carries now, its id when it has none. */
  name: string
}

export interface Hypothesis {
  id: string
  /** The agent whose hypothesis it is, empty on one no agent owns: such a row sits in the same
   *  pool under the same dedup, but owns no job, holds no quota and appears in no standings. */
  agent_id: string
  /** Who registered it, which is another credential than the agent's when one registered it for the agent. */
  actor: Actor
  platform_experiment_id: string
  text: string
  status: HypothesisStatus
  created_at: string
  /** When the verdict was last set; null while it has never been set. */
  status_changed_at: string | null
  /** Who last set the verdict; null while it has never been set. */
  status_actor: Actor | null
}

export type HypothesisLinkType = 'builds_on' | 'contradicts' | 'related'

// A row of GET /hypotheses and a node of the graph. `builds_on` names the outgoing builds_on
// targets; the objective root appears as the platform_experiment_id.
export interface HypothesisListItem extends Hypothesis {
  builds_on: string[]
  child_count: number
  finding_count: number
  comment_count: number
}

// actor and created_at say who made the link and when; agent_id is the agent it was made for.
interface HypothesisGraphEdge {
  from: string
  to: string
  type: HypothesisLinkType
  agent_id: string
  actor: Actor
  created_at: string
}

// A node of the graph: its newest finding, and the job testing it that did best on the ranking
// metric with the summary filed for that job. Each is absent when there is none.
export interface HypothesisGraphNode extends HypothesisListItem {
  finding?: HypothesisFinding
  best_job?: { id: string; metric: JobMetric; summary?: string; summary_due: boolean }
}

// GET /hypotheses/graph: one platform experiment's whole graph. `root` is the objective's id,
// equal to the platform_experiment_id.
export interface HypothesisGraph {
  root: string
  nodes: HypothesisGraphNode[]
  edges: HypothesisGraphEdge[]
}

// One neighbour on GET /hypotheses/{id}. When it is the objective root, hypothesis_id is the
// platform_experiment_id and text and status are empty. actor and created_at say who made the
// link and when; agent_id is the agent it was made for.
export interface HypothesisLink {
  type: HypothesisLinkType
  hypothesis_id: string
  text: string
  status: HypothesisStatus | ''
  agent_id: string
  actor: Actor
  created_at: string
}

export interface HypothesisLinkInput {
  type: HypothesisLinkType
  to: string
}

// A post-run write-up an agent filed after one of its jobs testing this hypothesis reached
// a terminal state — attached to the hypothesis (not the job), so it joins the shared,
// accumulated evidence trail for that claim. See POST /experiments/{id}/summary.
export interface HypothesisFinding {
  id: string
  hypothesis_id: string
  experiment_id: string
  agent_id: string
  /** Who filed it; agent_id is the job's agent, whose finding it is. */
  actor: Actor
  summary: string
  created_at: string
}

// A freeform, job-independent note against a hypothesis — as opposed to a finding, which
// requires a terminal job behind it.
export interface HypothesisComment {
  id: string
  hypothesis_id: string
  agent_id: string
  actor: Actor
  text: string
  created_at: string
}

// Response shape for GET /hypotheses/{id} — a hypothesis plus the jobs submitted against it,
// the findings filed against it, and its comments.
// Each list is one bounded page of a set that only grows; the matching *_count is the full
// size, so a short list and a truncated one are never confused. Page the rest with
// ?limit/?offset on GET /hypotheses/{id}.
export interface HypothesisWithJobs extends Hypothesis {
  jobs: Experiment[]
  job_count: number
  findings: HypothesisFinding[]
  finding_count: number
  comments: HypothesisComment[]
  comment_count: number
  links: { out: HypothesisLink[]; in: HypothesisLink[] }
}

// ---------------------------------------------------------------------------
// Accounts
// ---------------------------------------------------------------------------

export type OrgRole = 'owner' | 'member'

export interface Org {
  id: string
  name: string
  // The caller's own role. Absent for staff looking at an org they do not belong to.
  role?: OrgRole
  created_at: string
}

export interface Project {
  id: string
  org_id: string
  name: string
  created_at: string
}

// Who the session is and what it reaches: see GET /me. Staff reach every org.
export interface Me {
  kind: 'user' | 'staff' | 'key'
  user_id?: string
  name?: string
  orgs: Org[]
  projects: Project[]
}

// One person of an org: the id their identity provider gave them and the name it states, empty
// when it states none. The platform holds no email for them.
export interface OrgMember {
  org_id: string
  user_id: string
  name: string
  role: OrgRole
  joined_at: string
}

// A pending invite to an org. No read of the API returns its token.
export interface OrgInvite {
  id: string
  org_id: string
  role: OrgRole
  /** The person answerable for it: who made it, or the person behind the API key it was made with. */
  actor: Actor
  created_at: string
  expires_at: string
}

// An invite in the answer to creating it, the one place its token appears.
export interface CreatedOrgInvite extends OrgInvite {
  token: string
}

export interface APIKey {
  id: string
  org_id: string
  // Absent on a key that reaches the whole org.
  project_id?: string
  /** The person the key answers to: who created it, or the person behind the key it was created with. */
  actor: Actor
  // The key this one was created with, absent when a person created it.
  minted_by?: string
  name: string
  created_at: string
  revoked_at?: string
}

// An API key in the answer to creating it, the one place its token appears.
export interface CreatedAPIKey extends APIKey {
  token: string
}

// A cluster an org's jobs can be placed on: one it registered, or one of another org that the
// platform shares with every org, which comes without org_id and settings.
export interface OrgCluster extends ClusterSettings {
  name: string
  org_id?: string
  // The platform places every org's jobs on a shared cluster. Staff decide it.
  shared: boolean
  // Whether the org's jobs are placed on the cluster. An owner of the org turns it off and on,
  // for this org alone; jobs already running there finish.
  enabled: boolean
  created_at: string
  // The org registered the cluster and may change its settings.
  owned: boolean
  connected: boolean
  // The connected agent reports an autoscaler that adds nodes for waiting jobs.
  autoscaler_enabled: boolean
}

export type ClusterKind = 'kubernetes' | 'bare_metal'

// A registered cluster, the command that attaches it for each kind of runtime, and its runtime
// token, which is shown beside the command that asks for it on the machine.
export interface RegisteredCluster {
  name: string
  runtime_token: string
  attach_commands: Record<ClusterKind, string>
}

// What became of the machine the platform runs for a hosted agent or a coordinator, as its
// launcher reported it. Mirrors domain.AgentInstance.
export interface AgentInstance {
  // 'requested' until the launcher lists the machine asked for, then as it reports it, and
  // 'stopped' once no further machine is asked for.
  state: 'requested' | 'starting' | 'running' | 'stopped'
  // When the platform last saw the machine's command make progress. Absent until it first did.
  progress_at?: string
  // When the launcher first reported the machine now asked for.
  seen_at?: string
  // Machines that ended and were asked for again, of domain.MaxAgentInstanceRestarts.
  restarts: number
  // The last machine ended with no restart left.
  stopped: boolean
  // Why the last machine that ended was given up on, when, and the end of what it wrote. A
  // machine that was replaced leaves these set while state is running again.
  end_reason?: 'exited' | 'gone' | 'never_seen' | 'still_starting' | 'silent'
  exited_at?: string
  exit_log?: string
}

// The coordinator the platform runs for a platform experiment. Mirrors domain.HostedCoordinator.
export interface HostedCoordinator {
  launcher: string
  model: string
  instance: AgentInstance
}

// One agent's place in a platform experiment and what it was launched with.
export interface AgentSignup {
  platform_experiment_id: string
  agent_id: string
  name: string
  role: 'competitor' | 'baseline' | 'reviewer' | 'planner'
  // Whose credential added the agent.
  actor: Actor
  // true: the platform runs the agent on a machine of its own.
  hosted: boolean
  flavor?: string
  model?: string
  signed_up_at: string
  // Set once the agent said it has nothing left to do, which is final, with why.
  finished_at?: string
  finished_reason?: string
  // Absent for an agent the platform does not run.
  instance?: AgentInstance
}

// An agent in the answer to adding it. token is set for one the platform does not run.
export interface JoinedAgent extends AgentSignup {
  token?: string
}

/** The platform's GitHub App: where a person installs it and where they authorise it. */
export interface GitHubApp {
  slug: string
  client_id: string
  install_url: string
  authorize_url: string
}

export type GitHubAccountType = 'User' | 'Organization'

/**
 * An installation of the platform's GitHub App that an org connected: a GitHub account, with the
 * repositories its owner chose on GitHub. A platform experiment of the org may name any of them.
 */
export interface GitHubInstallation {
  org_id: string
  installation_id: number
  /** The GitHub user or organisation the App is installed on. */
  account_login: string
  /** Which of the two it is. The platform creates repositories on an organisation only. Absent while GitHub has not said. */
  account_type?: GitHubAccountType
  /** Who connected it: a user, the API key it was connected with, or staff. */
  connected_actor: Actor
  connected_at: string
}

/** An installation the person can reach, as a connect that must ask which one lists it. */
export interface ReachableInstallation {
  installation_id: number
  account_login: string
}

/** A repository a platform experiment of the org may name. */
export interface GitHubRepository {
  /** owner/name. */
  repository: string
  installation_id: number
  private: boolean
}
