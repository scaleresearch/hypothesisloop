import type { TypeKind } from '@/lib/units'

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
  // across every accelerator type — real hardware occupancy, not the used/budget AccH ratio
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
  // Where a job of the org tries the cluster, 1 first. Clusters of one place are tried together,
  // the one with the most room first. Absent while the org has the cluster turned off.
  place?: number
  // The connected agent reports an autoscaler that adds nodes for waiting jobs.
  autoscaler_enabled: boolean
  // When the cluster's agent was last heard from, within the last 30 days.
  last_contact_at?: string
  // Of a cluster the org registered: the image tag its agent runs, whether that is not the
  // platform's own, and what the platform last refused its agent with, until it accepts one.
  agent_version?: string
  version_skew?: boolean
  last_error?: string
  last_error_at?: string
}

export type ClusterKind = 'kubernetes' | 'bare_metal'

// A registered cluster, the command that attaches it for each kind of runtime, and its runtime
// token, which is shown beside the command that asks for it on the machine.
export interface RegisteredCluster {
  name: string
  runtime_token: string
  attach_commands: Record<ClusterKind, string>
  // Where the cluster's agents reach the platform and pull their images from, from the
  // control plane's settings.
  api_url: string
  metrics_url: string
  registry: string
}

// Live, per-cluster accelerator capacity: what is schedulable right now, read from the metrics
// store, and what an hour on one accelerator of each type costs in accelerator hours.
export interface ResourceCapacity {
  clusters: Array<{
    cluster_name: string
    // The cluster rents a machine when a job needs one. Nothing stands by: a type's total is the
    // most it rents at once and its available is what it may still rent.
    on_demand?: boolean
    // The cluster's cores and memory, summed over its nodes: in total and not asked for by
    // running jobs. Absent for a cluster that reported none.
    resources?: { cpu_cores_available: number; cpu_cores_total: number; memory_bytes_available: number; memory_bytes_total: number }
    // kind says what available and total count: devices, or for cpu cores.
    accelerators: Array<{ accelerator_type: string; kind: TypeKind; available: number; total: number; acch_per_hour: number }>
  }>
}
