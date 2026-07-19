// ---------------------------------------------------------------------------
// Enumerations
// ---------------------------------------------------------------------------

export enum ExperimentStatus {
  QUEUED = 'QUEUED',
  RUNNING = 'RUNNING',
  COMPLETED = 'COMPLETED',
  FAILED = 'FAILED',
  EVICTED = 'EVICTED',
  PROMOTED = 'PROMOTED',
}

// AcceleratorType is an open, operator-defined identifier (see openresearch.yaml's accelerator_types) — any
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

export interface MetricDataPoint {
  fraction_complete: number
  metric_name?: string
  metric_value?: number
  value?: number
  step?: number
  wall_time?: string
  recorded_at?: string
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

export interface Experiment {
  id: string
  parent_id?: string
  agent_id: string
  platform_experiment_id: string
  capacity_tier?: CapacityTier
  hypothesis_id: string
  hypothesis: string
  objective?: string
  status: ExperimentStatus
  estimated_cost_acch?: number
  actual_cost_acch?: number
  // Additional resource-dimension cost/usage, mirroring estimated_cost_acch/actual_cost_acch.
  // 0/absent means that dimension wasn't tracked for this job.
  estimated_cpu_core_hours?: number
  actual_cpu_core_hours?: number
  /** @deprecated RAM is no longer an hours-billed budget dimension — it's a physical fit-only
   * check at admission now. Kept for backward compat with old API responses; nothing populates
   * or debits this for new submissions. Do not render as a live budget/usage figure. */
  estimated_ram_gb_hours?: number
  /** @deprecated see estimated_ram_gb_hours. */
  estimated_storage_gb_hours?: number
  /** @deprecated see estimated_ram_gb_hours. */
  actual_ram_gb_hours?: number
  /** @deprecated see estimated_ram_gb_hours. */
  actual_storage_gb_hours?: number
  accelerator_type: AcceleratorType
  accelerator_count: number
  estimated_duration_hours?: number
  // The job's own DSL — image, command, resources, accelerator count/type, distributed topology.
  job?: JobSpec
  created_at: string
  started_at?: string
  completed_at?: string
  eviction_reason?: string
  not_admitted_reason?: string
  final_metric_value?: number
  // Lineage (Domain 7)
  code_ref?: string
  config_hash?: string
  data_ref?: string
  /** @deprecated pre-JobSpec-DSL field, no longer sent by the backend; see `job.image` instead. */
  env_image?: string
}

export interface MetricDefinition {
  key: string
  direction: 'maximize' | 'minimize'
  description?: string
}

export interface PlatformExperiment {
  id: string
  name: string
  description?: string
  budget_accelerator_hours: number
  // Optional additional resource budgets, tracked the same way as budget_accelerator_hours. 0/absent
  // means that dimension isn't tracked for this platform experiment.
  budget_cpu_core_hours?: number
  /** @deprecated RAM is no longer an hours-billed budget dimension — it's a physical fit-only
   * check at admission now. Kept for backward compat with old API responses; nothing debits or
   * enforces this for new platform experiments. Do not render as a live budget figure. */
  budget_ram_gb_hours?: number
  /** @deprecated see budget_ram_gb_hours. */
  budget_storage_gb_hours?: number
  max_agents: number
  starts_at?: string
  ends_at?: string
  status: PlatformExperimentStatus
  phase: number                       // 1 or 2 (Domain 10)
  phase2_triggered_at?: string        // ISO timestamp when phase 2 started
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
  // Additional resource dimensions — 0/absent means the platform experiment doesn't track
  // that dimension.
  guaranteed_cpu_core_hours?: number
  burst_cpu_core_hours?: number
  used_guaranteed_cpu_core_h?: number
  used_burst_cpu_core_h?: number
  /** @deprecated RAM/storage guaranteed/burst/used fields below are frozen — RAM/storage are no
   * longer hours-billed budget dimensions (physical fit-only check at admission now). Kept for
   * backward compat with old API responses; nothing debits these for new submissions. Do not
   * render as a live budget/usage figure. */
  guaranteed_ram_gb_hours?: number
  burst_ram_gb_hours?: number
  used_guaranteed_ram_gb_h?: number
  used_burst_ram_gb_h?: number
  guaranteed_storage_gb_hours?: number
  burst_storage_gb_hours?: number
  used_guaranteed_storage_gb_h?: number
  used_burst_storage_gb_h?: number
}

// ---------------------------------------------------------------------------
// Agent
// ---------------------------------------------------------------------------

export interface Agent {
  id: string
  name: string
  periods_active: number
  performance_score: number
  top3_count: number       // number of top-3 placements ever (drives +25% bonus)
  created_at: string
}

// ---------------------------------------------------------------------------
// Credit ledger
// ---------------------------------------------------------------------------

export interface CreditLedgerEntry {
  id: string
  agent_id: string
  amount: number
  reason: string
  experiment_id?: string
  period: string
  created_at: string
}

export interface AgentBalance {
  agent_id: string
  balance: number
  period: string
  experience_bonus: number
  performance_bonus: number
  borrowing_used: number
}

// A registered target cluster and whether its cluster-agent is currently connected.
// Connectivity is a heartbeat the agent leaves on every desired-state poll (~2s) — the
// control plane never dials the cluster itself, so "connected" here means "we've heard
// from it recently," not "we can reach it."
export interface ClusterInfo {
  cluster_name: string
  last_seen_at: string
  connected: boolean
}

export interface ClustersResponse {
  clusters: ClusterInfo[]
}

// ---------------------------------------------------------------------------
// Allotment
// ---------------------------------------------------------------------------

export interface AllotmentResult {
  new_period: number
  prev_period: number
  winner_agent?: string
  winner_metric?: number
  winner_experiment_id?: string
  allotments: Array<{ agent_id: string; credits: number; performance_score: number }>
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

export interface Hypothesis {
  id: string
  agent_id: string
  platform_experiment_id: string
  text: string
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

// Response shape for GET /registry/hypotheses/{id} — a hypothesis plus every job
// (experiment) submitted against it, and every finding filed against it, so far.
export interface HypothesisWithJobs extends Hypothesis {
  jobs: Experiment[]
  findings: HypothesisFinding[]
}
