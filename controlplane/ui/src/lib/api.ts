import type {
  ClustersResponse,
  AgentBalance,
  Agent,
  Experiment,
  MetricDataPoint,
  CreditLedgerEntry,
  LineageNode,
  PlatformExperiment,
  AgentQuota,
  MetricDefinition,
  AgentMetricSeries,
  Hypothesis,
  HypothesisWithJobs,
} from '@/types'

const QUOTA_URL = process.env.NEXT_PUBLIC_QUOTA_URL || 'http://localhost:8081'
const REGISTRY_URL = process.env.NEXT_PUBLIC_REGISTRY_URL || 'http://localhost:8083'
const SCHED_URL = process.env.NEXT_PUBLIC_SCHED_URL || 'http://localhost:8082'

async function apiFetch<T>(url: string): Promise<T> {
  const res = await fetch(url, { cache: 'no-store' })
  if (!res.ok) {
    throw new Error(`API error ${res.status} from ${url}`)
  }
  return res.json() as Promise<T>
}

// ---------------------------------------------------------------------------
// Cluster / Quota
// ---------------------------------------------------------------------------

// Registered target clusters and whether each one's cluster-agent is currently connected
// (has polled desired-state recently). The control plane never dials a cluster itself.
export function fetchClusters(): Promise<ClustersResponse> {
  return apiFetch<ClustersResponse>(`${SCHED_URL}/internal/clusters`)
}

export function fetchAgentBalances(): Promise<AgentBalance[]> {
  return apiFetch<AgentBalance[]>(`${QUOTA_URL}/balances`)
}

export function fetchAgents(): Promise<Agent[]> {
  return apiFetch<Agent[]>(`${QUOTA_URL}/agents`)
}

export function fetchAgentLedger(agentID: string): Promise<CreditLedgerEntry[]> {
  return apiFetch<CreditLedgerEntry[]>(`${QUOTA_URL}/ledger/${agentID}`)
}

// ---------------------------------------------------------------------------
// Registry / Experiments
// ---------------------------------------------------------------------------

export interface ExperimentsParams {
  status?: string
  tier?: string
  agent_id?: string
  platform_experiment_id?: string
  limit?: number
  offset?: number
}

export function fetchExperiments(params?: ExperimentsParams): Promise<Experiment[]> {
  const url = new URL(`${REGISTRY_URL}/registry/experiments`)
  if (params) {
    for (const [k, v] of Object.entries(params)) {
      if (v !== undefined && v !== '') url.searchParams.set(k, String(v))
    }
  }
  return apiFetch<Experiment[]>(url.toString())
}

export function fetchExperiment(id: string): Promise<Experiment> {
  return apiFetch<Experiment>(`${REGISTRY_URL}/registry/experiments/${id}`)
}

export function fetchExperimentLineage(id: string): Promise<LineageNode[]> {
  return apiFetch<LineageNode[]>(`${REGISTRY_URL}/registry/experiments/${id}/lineage`)
}

export function fetchExperimentMetrics(id: string): Promise<MetricDataPoint[]> {
  return apiFetch<MetricDataPoint[]>(`${REGISTRY_URL}/registry/experiments/${id}/metrics`)
}

// Full metric history for every competing job in a platform experiment — one series per
// job/agent — for a leaderboard/competition-over-time dashboard.
export function fetchPlatformExperimentTimeseries(
  platformExpID: string,
  metricName: string,
  lookbackHours = 24,
): Promise<{ series: AgentMetricSeries[] }> {
  return apiFetch<{ series: AgentMetricSeries[] }>(
    `${REGISTRY_URL}/registry/platform-experiments/${platformExpID}/metrics-timeseries?metric_name=${encodeURIComponent(metricName)}&lookback_hours=${lookbackHours}`,
  )
}

// ---------------------------------------------------------------------------
// Hypotheses
// ---------------------------------------------------------------------------

// Hypotheses are scoped to a single platform experiment's shared idea pool — there is no
// unscoped/global listing on the backend. Callers that want hypotheses across every platform
// experiment (e.g. the /hypotheses page) should fetch platform experiments first and call
// this once per ID; see fetchAllHypotheses below for that aggregation.
export function fetchHypotheses(platformExperimentID: string): Promise<Hypothesis[]> {
  return apiFetch<Hypothesis[]>(
    `${REGISTRY_URL}/registry/hypotheses?platform_experiment_id=${encodeURIComponent(platformExperimentID)}`,
  )
}

// Fetches the hypothesis pool for every given platform experiment and merges them into one
// list, most recent first — powers the unscoped /hypotheses view (filterable by platform
// experiment client-side) without requiring the backend to support a global listing.
export async function fetchAllHypotheses(platformExperimentIDs: string[]): Promise<Hypothesis[]> {
  // allSettled, not all: one stale/unreachable platform experiment (e.g. deleted after this
  // list was fetched) must not blank out every other experiment's hypotheses with a single
  // failed-fetch error.
  const results = await Promise.allSettled(platformExperimentIDs.map(id => fetchHypotheses(id)))
  const lists: Hypothesis[][] = []
  for (const r of results) {
    if (r.status === 'fulfilled' && Array.isArray(r.value)) {
      lists.push(r.value)
    } else if (r.status === 'rejected') {
      console.error('fetchAllHypotheses: failed to fetch one platform experiment\'s hypotheses', r.reason)
    }
  }
  return lists.flat().sort((a, b) => b.created_at.localeCompare(a.created_at))
}

export function fetchHypothesis(id: string): Promise<HypothesisWithJobs> {
  return apiFetch<HypothesisWithJobs>(`${REGISTRY_URL}/registry/hypotheses/${id}`)
}

export interface AllotmentResult {
  new_period: number
  prev_period: number
  winner_agent?: string
  winner_metric?: number
  winner_experiment_id?: string
  allotments: Array<{ agent_id: string; credits: number; performance_score: number }>
}

export async function beginNextPeriod(): Promise<AllotmentResult> {
  const res = await fetch(`${QUOTA_URL}/quota/allotment`, { method: 'POST', cache: 'no-store' })
  if (!res.ok) throw new Error(`allotment failed: ${res.status}`)
  return res.json()
}

export async function cancelExperiment(id: string): Promise<void> {
  const res = await fetch(`${SCHED_URL}/experiments/${id}/cancel`, { method: 'POST', cache: 'no-store' })
  if (!res.ok) throw new Error(`cancel failed: ${res.status}`)
}

// ---------------------------------------------------------------------------
// Platform Experiments
// ---------------------------------------------------------------------------

export interface CreatePlatformExperimentRequest {
  name: string
  description?: string
  budget_t4_hours: number
  // Optional additional CPU budget, tracked the same way as budget_t4_hours
  // (guaranteed/burst split, debited at submission, refunded on completion). 0/omitted
  // means that dimension isn't tracked for this platform experiment.
  budget_cpu_core_hours?: number
  /** @deprecated RAM is no longer an hours-billed budget dimension — physical fit-only check at
   * admission now. Always sent as 0 by the UI; kept in the request type only because the backend
   * still accepts/echoes the field for backward compat. */
  budget_ram_gb_hours?: number
  /** @deprecated see budget_ram_gb_hours. */
  budget_storage_gb_hours?: number
  max_agents: number
  metrics?: MetricDefinition[]
  report_interval_seconds?: number
  starts_at: string
  ends_at: string
}

export async function createPlatformExperiment(req: CreatePlatformExperimentRequest): Promise<PlatformExperiment> {
  const res = await fetch(`${QUOTA_URL}/platform-experiments`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(req),
    cache: 'no-store',
  })
  if (!res.ok) throw new Error(`create failed: ${res.status}`)
  return res.json()
}

export function fetchPlatformExperiments(status?: string): Promise<PlatformExperiment[]> {
  const url = new URL(`${QUOTA_URL}/platform-experiments`)
  if (status) url.searchParams.set('status', status)
  return apiFetch<PlatformExperiment[]>(url.toString())
}

export function fetchPlatformExperiment(id: string): Promise<PlatformExperiment> {
  return apiFetch<PlatformExperiment>(`${QUOTA_URL}/platform-experiments/${id}`)
}

export async function signupPlatformExperiment(id: string, agentID: string): Promise<{ status: string }> {
  const res = await fetch(`${QUOTA_URL}/platform-experiments/${id}/signup`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ agent_id: agentID }),
    cache: 'no-store',
  })
  if (!res.ok) throw new Error(`signup failed: ${res.status}`)
  return res.json()
}

export function fetchAgentQuota(agentID: string, platformExpID: string): Promise<AgentQuota> {
  return apiFetch<AgentQuota>(`${QUOTA_URL}/quota/${agentID}/experiment/${platformExpID}`)
}

export function fetchPlatformExperimentQuotas(platformExpID: string): Promise<AgentQuota[]> {
  return apiFetch<AgentQuota[]>(`${QUOTA_URL}/platform-experiments/${platformExpID}/quotas`)
}

// Resource pricing reference data (GPU type rates, CPU/RAM/storage flat rates) — fetched
// from the backend instead of hardcoded, so the UI never drifts from the operator's config.
export interface ResourceCatalog {
  gpu_types: Array<{ name: string; t4h_rate: number }>
  cpu_core_hour_rate: number
  ram_gb_hour_rate: number
  storage_gb_hour_rate: number
}

export function fetchResourceCatalog(): Promise<ResourceCatalog> {
  return apiFetch<ResourceCatalog>(`${QUOTA_URL}/resource-catalog`)
}

export async function updatePlatformExperiment(id: string, req: CreatePlatformExperimentRequest): Promise<PlatformExperiment> {
  const res = await fetch(`${QUOTA_URL}/platform-experiments/${id}`, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(req),
    cache: 'no-store',
  })
  if (!res.ok) throw new Error(`update failed: ${res.status}`)
  return res.json()
}

export interface DonationRequest {
  id: string
  agent_id: string
  agent_name?: string
  credits_want: number
  reason: string
  status: 'open' | 'fulfilled' | 'cancelled'
  created_at: string
}

export function fetchDonations(status?: string): Promise<DonationRequest[]> {
  const url = new URL(`${QUOTA_URL}/donations`)
  if (status) url.searchParams.set('status', status)
  return apiFetch<DonationRequest[]>(url.toString())
}

export function fetchExperimentsByPlatformExperiment(platformExpID: string): Promise<Experiment[]> {
  const url = new URL(`${REGISTRY_URL}/registry/experiments`)
  url.searchParams.set('platform_experiment_id', platformExpID)
  return apiFetch<Experiment[]>(url.toString())
}

export interface Phase2Status {
  phase: number
  phase2_triggered_at?: string
  active_agents: string[]
  held_agents: string[]
  boundary_fraction: number
}

export function fetchPhase2Status(platformExpID: string): Promise<Phase2Status> {
  return apiFetch<Phase2Status>(`${QUOTA_URL}/platform-experiments/${platformExpID}/phase2-status`)
}
