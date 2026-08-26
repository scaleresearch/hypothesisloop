// ---------------------------------------------------------------------------
// Enumerations
// ---------------------------------------------------------------------------

// Exactly domain.ValidExperimentStatus's set (shared/domain/constants.go). It used to omit
// SUBMITTED/ADMITTED/REJECTED -- which every page then had to reach with a string cast -- and
// carry PROMOTED, which the backend cannot produce.
export enum ExperimentStatus {
  SUBMITTED = 'SUBMITTED',
  QUEUED = 'QUEUED',
  ADMITTED = 'ADMITTED',
  RUNNING = 'RUNNING',
  COMPLETED = 'COMPLETED',
  FAILED = 'FAILED',
  EVICTED = 'EVICTED',
  REJECTED = 'REJECTED',
}

// AcceleratorType is an open, operator-defined identifier (see hypothesisloop.yaml's accelerator_types) — any
// vendor's model name is valid (NVIDIA, AMD, ...), not a fixed set. Fetch the live catalog
// (name + rate) via fetchResourceCatalog() instead of hardcoding known values here.
export type AcceleratorType = string

export enum CapacityTier {
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
export interface MetricSeriesPoint {
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
export interface JobSpec {
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
export interface PhaseDetail {
  reason?: string
  message?: string
  restart_count?: number
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
  platform_experiment_id: string
  project_id: string
  cluster_name?: string
  capacity_tier?: CapacityTier
  hypothesis_id: string
  hypothesis: string
  objective?: string
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

// SubmitterPolicy restricts who may submit a hypothesis or a job: a real person, an autonomous
// agent, or both. Empty/absent means 'mixed' — every platform experiment created before this
// field existed keeps accepting both, unchanged.
export type SubmitterPolicy = 'mixed' | 'human_only' | 'agent_only'

export interface PlatformExperiment {
  id: string
  name: string
  description?: string
  budget_accelerator_hours: number
  // Who may submit a hypothesis / a job in this platform experiment. Independent settings — an
  // operator may want humans-only ideation with any agent free to run jobs against it, or the
  // reverse. Locked once the experiment is running (see the create/edit form).
  hypothesis_submit_policy?: SubmitterPolicy
  job_submit_policy?: SubmitterPolicy
  max_agents: number
  // Accelerators-in-flight (SUBMITTED+RUNNING) this experiment may hold at once. Absent uses the
  // platform default (quota.default_max_concurrent_accelerators).
  max_concurrent_accelerators?: number
  starts_at?: string
  ends_at?: string
  status: PlatformExperimentStatus
  stages: Array<{ length_pct: number; evict_pct: number; max_job_hours?: number }>  // the elimination ladder
  current_stage: number               // 1-based index into stages
  signup_count: number
  signed_up_agents?: string[]
  metrics?: MetricDefinition[]
  report_interval_seconds?: number
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

export interface AgentQuota {
  id: string
  agent_id: string
  platform_experiment_id: string
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
  // "agent" (default) or "human" — see domain.AgentKind. Optional because older API responses
  // predate the field; treat a missing value as "agent".
  kind?: 'agent' | 'human'
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
  // survive a rename (cluster_settings, tried_clusters) keys on this, never on cluster_name.
  cluster_id: string
  last_seen_at: string
  connected: boolean
  // Actual busy-vs-idle chip counts from the cluster's most recent reconcile snapshot, summed
  // across every accelerator flavor — real hardware occupancy, not the used/budget AccH ratio
  // shown per platform experiment. Both are 0 when no live snapshot is within the freshness
  // window (e.g. a disconnected cluster).
  accelerator_busy: number
  accelerator_total: number
  // Whether this cluster sits behind a native autoscaler (cluster-autoscaler / Karpenter),
  // operator-set via AUTOSCALER_ENABLED on the agent deployment. Fail-closed: false means the
  // scheduler never speculatively submits onto this cluster.
  autoscaler_enabled: boolean
}

export interface ClustersResponse {
  clusters: ClusterInfo[]
}

// Operator overrides for one cluster's autoscaler-speculation behaviour — see PUT
// /clusters/{cluster_id}/settings. Null fields mean "use the global scheduler default."
export interface ClusterSettings {
  cluster_id: string
  scale_up_timeout_seconds?: number | null
  max_speculative_accelerators?: number | null
}

// ---------------------------------------------------------------------------
// Lineage
// ---------------------------------------------------------------------------

export interface LineageNode {
  id: string
  hypothesis: string
  status: ExperimentStatus
  final_metric_value?: number
  created_at: string
}

// ---------------------------------------------------------------------------
// Hypotheses
// ---------------------------------------------------------------------------

// The owning agent's own verdict on its claim — see POST /hypotheses/{id}/status. Only
// the agent named in `agent_id` may ever change it.
export type HypothesisStatus = 'open' | 'confirmed' | 'inconclusive'

// Who put a row in the pool. Human rows come from the UI form and carry `author` instead of
// `agent_id`; exactly one of the two is ever set. Both sit in the same pool under the same dedup,
// but a human row owns no job, holds no quota, and appears in no standings.
export type HypothesisSource = 'agent' | 'human'

export interface Hypothesis {
  id: string
  /** Empty on a human-submitted row — the owner column names nobody. */
  agent_id: string
  source: HypothesisSource
  /** The name a human typed. There is no auth: a claim, not an identity, exactly as agent_id is. */
  author: string
  platform_experiment_id: string
  text: string
  status: HypothesisStatus
  created_at: string
}

// A post-run write-up an agent filed after one of its jobs testing this hypothesis reached
// a terminal state — attached to the hypothesis (not the job), so it joins the shared,
// accumulated evidence trail for that claim. See POST /experiments/{id}/summary.
export interface HypothesisFinding {
  id: string
  hypothesis_id: string
  experiment_id: string
  agent_id: string
  summary: string
  created_at: string
}

// A freeform, job-independent note against a hypothesis — as opposed to a finding, which
// requires a terminal job behind it.
export interface HypothesisComment {
  id: string
  hypothesis_id: string
  agent_id: string
  source: HypothesisSource
  author: string
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
}
