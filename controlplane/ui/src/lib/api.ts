import type {
  ClustersResponse,
  Agent,
  Experiment,
  MetricDataPoint,
  LineageNode,
  PlatformExperiment,
  AgentQuota,
  MetricDefinition,
  AgentMetricSeries,
  Hypothesis,
  HypothesisComment,
  HypothesisWithJobs,
} from '@/types'

// One API, one base URL — quota, scheduler and registry operations are all served together.
const API_URL = process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8081'

async function apiFetch<T>(url: string): Promise<T> {
  const res = await fetch(url, { cache: 'no-store' })
  if (!res.ok) {
    throw new Error(`API error ${res.status} from ${url}`)
  }
  return res.json() as Promise<T>
}

// A paginated list response: the page of items plus the total match count across all pages,
// read off the X-Total-Count response header — see the unified pagination convention shared by
// every list endpoint (?limit, ?offset, ?sort; total in X-Total-Count).
export interface Page<T> {
  items: T[]
  total: number
}

// The largest page every list endpoint will serve; asking for more is clamped server-side.
export const MAX_LIST_PAGE_SIZE = 200

async function apiFetchPage<T>(url: string): Promise<Page<T>> {
  const res = await fetch(url, { cache: 'no-store' })
  if (!res.ok) {
    throw new Error(`API error ${res.status} from ${url}`)
  }
  const items = (await res.json()) as T[]
  const totalHeader = res.headers.get('X-Total-Count')
  const total = totalHeader != null ? Number(totalHeader) : items.length
  return { items, total }
}

// ---------------------------------------------------------------------------
// Cluster / Quota
// ---------------------------------------------------------------------------

// Registered target clusters and whether each one's cluster-agent is currently connected
// (has polled desired-state recently). The control plane never dials a cluster itself.
export function fetchClusters(): Promise<ClustersResponse> {
  return apiFetch<ClustersResponse>(`${API_URL}/internal/clusters`)
}


export interface AgentsParams {
  limit?: number
  offset?: number
}

export function fetchAgentsPage(params?: AgentsParams): Promise<Page<Agent>> {
  const url = new URL(`${API_URL}/agents`)
  if (params) {
    for (const [k, v] of Object.entries(params)) {
      if (v !== undefined) url.searchParams.set(k, String(v))
    }
  }
  return apiFetchPage<Agent>(url.toString())
}

// The first page at the server's own ceiling, for the id->name lookups that decorate other
// pages' rows. Callers that display agents as their subject must page instead — see
// fetchAgentsPage.
export function fetchAgents(): Promise<Agent[]> {
  return fetchAgentsPage({ limit: MAX_LIST_PAGE_SIZE }).then(p => p.items)
}


// ---------------------------------------------------------------------------
// Registry / Experiments
// ---------------------------------------------------------------------------

// Field names are sent verbatim as query params, so they must match the API's own names.
export interface ExperimentsParams {
  status?: string
  agent?: string
  platform_experiment_id?: string
  /** Substring match against hypothesis/objective/theory. */
  search?: string
  limit?: number
  offset?: number
  /** "created_at" | "priority_score" | "status", optionally prefixed with "-" for descending. */
  sort?: string
}

export function fetchExperimentsPage(params?: ExperimentsParams): Promise<Page<Experiment>> {
  const url = new URL(`${API_URL}/experiments`)
  if (params) {
    for (const [k, v] of Object.entries(params)) {
      if (v !== undefined && v !== '') url.searchParams.set(k, String(v))
    }
  }
  return apiFetchPage<Experiment>(url.toString())
}

export function fetchExperiments(params?: ExperimentsParams): Promise<Experiment[]> {
  return fetchExperimentsPage(params).then(p => p.items)
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
  // ('policy'). Derived server-side from the same tally; see shared/domain/fault_class.go.
  evictions_by_class: Record<string, number>
}

export function fetchExperimentStats(params?: Omit<ExperimentsParams, 'limit' | 'offset' | 'sort'>): Promise<ExperimentStats> {
  const url = new URL(`${API_URL}/experiments/stats`)
  if (params) {
    for (const [k, v] of Object.entries(params)) {
      if (v !== undefined && v !== '') url.searchParams.set(k, String(v))
    }
  }
  return apiFetch<ExperimentStats>(url.toString())
}

export function fetchExperiment(id: string): Promise<Experiment> {
  return apiFetch<Experiment>(`${API_URL}/experiments/${id}`)
}


export function fetchExperimentMetrics(id: string): Promise<MetricDataPoint[]> {
  return apiFetch<MetricDataPoint[]>(`${API_URL}/experiments/${id}/metrics`)
}

// The job's most recently reported stdout/stderr tail. For a failed job this is usually the
// only place the error itself appears — the record carries a status and a phase_detail reason,
// but the traceback lives here.
export function fetchExperimentLogs(id: string, n = 200): Promise<string[]> {
  return apiFetch<string[]>(`${API_URL}/experiments/${id}/logs?n=${n}`)
}

// Full metric history for every competing job in a platform experiment — one series per
// job/agent — for a leaderboard/competition-over-time dashboard.
export function fetchPlatformExperimentTimeseries(
  platformExpID: string,
  metricName: string,
  lookbackHours = 24,
): Promise<{ series: AgentMetricSeries[] }> {
  return apiFetch<{ series: AgentMetricSeries[] }>(
    `${API_URL}/platform-experiments/${platformExpID}/metrics-timeseries?metric_name=${encodeURIComponent(metricName)}&lookback_hours=${lookbackHours}`,
  )
}

// ---------------------------------------------------------------------------
// Hypotheses
// ---------------------------------------------------------------------------

export interface HypothesesParams {
  /** Omit for the global, cross-platform-experiment view. */
  platform_experiment_id?: string
  agent?: string
  status?: string
  limit?: number
  offset?: number
}

export function fetchHypothesesPage(params?: HypothesesParams): Promise<Page<Hypothesis>> {
  const url = new URL(`${API_URL}/hypotheses`)
  if (params) {
    for (const [k, v] of Object.entries(params)) {
      if (v !== undefined && v !== '') url.searchParams.set(k, String(v))
    }
  }
  return apiFetchPage<Hypothesis>(url.toString())
}

export function fetchHypothesis(id: string): Promise<HypothesisWithJobs> {
  return apiFetch<HypothesisWithJobs>(`${API_URL}/hypotheses/${id}`)
}

// Registers a human's idea into a platform experiment's pool. `author` is the name the person
// typed, sent instead of an agent_id — exactly one of the two is accepted, and there is no auth
// behind either. The row lands in the same pool, under the same dedup, as an agent's: text
// equivalent to an existing claim returns that row rather than adding a second.
export async function registerHumanHypothesis(
  platformExperimentID: string, author: string, text: string,
): Promise<Hypothesis> {
  const res = await fetch(`${API_URL}/hypotheses`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ platform_experiment_id: platformExperimentID, author, text }),
    cache: 'no-store',
  })
  if (!res.ok) throw new Error(await res.text())
  return res.json()
}

export async function addHumanHypothesisComment(
  hypothesisID: string, author: string, text: string,
): Promise<HypothesisComment> {
  const res = await fetch(`${API_URL}/hypotheses/${hypothesisID}/comments`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ author, text }),
    cache: 'no-store',
  })
  if (!res.ok) throw new Error(await res.text())
  return res.json()
}

export async function cancelExperiment(id: string): Promise<void> {
  const res = await fetch(`${API_URL}/experiments/${id}/cancel`, { method: 'POST', cache: 'no-store' })
  if (!res.ok) throw new Error(`cancel failed: ${res.status}`)
}

// ---------------------------------------------------------------------------
// Platform Experiments
// ---------------------------------------------------------------------------

export interface CreatePlatformExperimentRequest {
  name: string
  description?: string
  budget_accelerator_hours: number
  // Optional CPU budget. Current PostgreSQL estimates and settled metrics observations
  // contribute to its guaranteed/burst usage. 0/omitted means it is not tracked.
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
  /** Elimination ladder; omitted means the platform default. See docs/stages.md. */
  stages?: Stage[]
  starts_at: string
  ends_at: string
}

export async function createPlatformExperiment(req: CreatePlatformExperimentRequest): Promise<PlatformExperiment> {
  const res = await fetch(`${API_URL}/platform-experiments`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(req),
    cache: 'no-store',
  })
  if (!res.ok) throw new Error(`create failed: ${res.status}`)
  return res.json()
}

export interface PlatformExperimentsParams {
  status?: string
  /** Search over name/description. */
  q?: string
  limit?: number
  offset?: number
  /** "created_at" | "name" | "status", optionally prefixed with "-" for descending. */
  sort?: string
}

export function fetchPlatformExperimentsPage(params?: PlatformExperimentsParams): Promise<Page<PlatformExperiment>> {
  const url = new URL(`${API_URL}/platform-experiments`)
  if (params) {
    for (const [k, v] of Object.entries(params)) {
      if (v !== undefined && v !== '') url.searchParams.set(k, String(v))
    }
  }
  return apiFetchPage<PlatformExperiment>(url.toString())
}

// The first page at the server's own ceiling, for the id->name lookups and filter dropdowns that
// decorate other pages. The list default is deliberately small (see the API's own docs), so this
// asks explicitly rather than silently showing 20. Callers that display platform experiments as
// their subject must page instead — see fetchPlatformExperimentsPage.
export function fetchPlatformExperiments(status?: string): Promise<PlatformExperiment[]> {
  return fetchPlatformExperimentsPage({ ...(status ? { status } : {}), limit: MAX_LIST_PAGE_SIZE })
    .then(p => p.items)
}

export function fetchPlatformExperiment(id: string): Promise<PlatformExperiment> {
  return apiFetch<PlatformExperiment>(`${API_URL}/platform-experiments/${id}`)
}

export async function signupPlatformExperiment(id: string, agentID: string): Promise<{ status: string }> {
  const res = await fetch(`${API_URL}/platform-experiments/${id}/signup`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ agent_id: agentID }),
    cache: 'no-store',
  })
  if (!res.ok) throw new Error(`signup failed: ${res.status}`)
  return res.json()
}

export function fetchAgentQuota(agentID: string, platformExpID: string): Promise<AgentQuota> {
  return apiFetch<AgentQuota>(`${API_URL}/platform-experiments/${platformExpID}/quotas/${agentID}`)
}

export function fetchPlatformExperimentQuotas(platformExpID: string): Promise<AgentQuota[]> {
  return apiFetch<AgentQuota[]>(`${API_URL}/platform-experiments/${platformExpID}/quotas`)
}

// Resource pricing reference data (Accelerator type rates, CPU/RAM/storage flat rates) — fetched
// from the backend instead of hardcoded, so the UI never drifts from the operator's config.
export interface ResourceCatalog {
  accelerator_types: Array<{ name: string; acch_rate: number }>
  cpu_core_hour_rate: number
  ram_gb_hour_rate: number
  storage_gb_hour_rate: number
}


// Live, per-cluster accelerator capacity — what's actually schedulable right now, as opposed to
// fetchResourceCatalog's static type/rate list. Same numbers the scheduler's own admission loop
// reads (metricsdb.LiveClusterAcceleratorCapacity), just exposed for display.
export interface ResourceCapacity {
  clusters: Array<{
    cluster_name: string
    accelerators: Array<{ accelerator_type: string; available: number; total: number }>
  }>
}

export function fetchResourceCapacity(): Promise<ResourceCapacity> {
  return apiFetch<ResourceCapacity>(`${API_URL}/resource-catalog/capacity`)
}

export async function updatePlatformExperiment(id: string, req: CreatePlatformExperimentRequest): Promise<PlatformExperiment> {
  const res = await fetch(`${API_URL}/platform-experiments/${id}`, {
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

export interface DonationsParams {
  status?: string
  limit?: number
  offset?: number
}

export function fetchDonationsPage(params?: DonationsParams): Promise<Page<DonationRequest>> {
  const url = new URL(`${API_URL}/donations`)
  if (params) {
    for (const [k, v] of Object.entries(params)) {
      if (v !== undefined && v !== '') url.searchParams.set(k, String(v))
    }
  }
  return apiFetchPage<DonationRequest>(url.toString())
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

/** Deliberately carries no per-agent rank or standings — see docs/stages.md. */
export interface StagesStatus {
  stages: Stage[]
  current_stage: number
  progress: number
  next_boundary_progress: number
  advances: StageAdvance[]
  active_agents: string[]
  cut_agents: AgentCut[]
}

export function fetchStages(platformExpID: string): Promise<StagesStatus> {
  return apiFetch<StagesStatus>(`${API_URL}/platform-experiments/${platformExpID}/stages`)
}
