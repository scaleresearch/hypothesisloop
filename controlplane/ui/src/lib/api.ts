import type {
  ClustersResponse,
  ClusterSettings,
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
  HypothesisFinding,
  HypothesisGraph,
  HypothesisLinkInput,
  HypothesisLinkType,
  HypothesisListItem,
  HypothesisStatus,
  HypothesisWithJobs,
} from '@/types'

import { isZeroDate } from '@/lib/format'

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

// The largest page any list endpoint will serve; asking for more is clamped server-side, and
// the job list clamps to 50.
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

// Per-cluster autoscaler-speculation overrides — null fields mean "use the global scheduler
// default." Keyed by cluster_id (survives a rename), not cluster_name.
export function fetchClusterSettings(clusterID: string): Promise<ClusterSettings> {
  return apiFetch<ClusterSettings>(`${API_URL}/clusters/${clusterID}/settings`)
}

export async function putClusterSettings(
  clusterID: string,
  req: { scale_up_timeout_seconds?: number | null; max_speculative_accelerators?: number | null },
): Promise<ClusterSettings> {
  const res = await fetch(`${API_URL}/clusters/${clusterID}/settings`, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(req),
    cache: 'no-store',
  })
  if (!res.ok) throw new Error(`update cluster settings failed: ${res.status}`)
  return res.json()
}


export interface AgentsParams {
  platform_experiment_id?: string
  /** Substring match against id and name. */
  q?: string
  limit?: number
  offset?: number
}

export function fetchAgentsPage(params?: AgentsParams): Promise<Page<Agent>> {
  const url = new URL(`${API_URL}/agents`)
  if (params) {
    for (const [k, v] of Object.entries(params)) {
      if (v !== undefined && v !== '') url.searchParams.set(k, String(v))
    }
  }
  return apiFetchPage<Agent>(url.toString())
}

// Whole-set figures for the filters fetchAgentsPage accepts, aggregated server-side.
export interface AgentStats {
  total: number
  top3_eligible: number
  avg_performance_score: number
}

export function fetchAgentStats(platformExperimentID: string): Promise<AgentStats> {
  const url = new URL(`${API_URL}/agents/stats`)
  if (platformExperimentID) url.searchParams.set('platform_experiment_id', platformExperimentID)
  return apiFetch<AgentStats>(url.toString())
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
  /** Only COMPLETED jobs whose agent filed no summary. */
  needs_summary?: boolean
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

// The ids of the completed jobs no summary was filed for, at most the 50 newest.
export function fetchUnsummarizedJobIDs(params: Pick<ExperimentsParams, 'agent' | 'platform_experiment_id'>): Promise<Set<string>> {
  return fetchExperimentsPage({ ...params, needs_summary: true, limit: 50 }).then(p => new Set(p.items.map(j => j.id)))
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
  // accelerator_count summed over SUBMITTED and RUNNING jobs.
  in_flight_accelerators: number
  // Present only when group_by was asked for: the same stats per agent or hypothesis id.
  groups?: Record<string, ExperimentStats>
}

export function fetchExperimentStats(
  params?: Omit<ExperimentsParams, 'limit' | 'offset' | 'sort'> & { group_by?: 'agent' | 'hypothesis' },
): Promise<ExperimentStats> {
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

// Metric history of every job in a platform experiment over its own run, one series per job. The
// window reaches from the start to the end, or to now while it runs: each series comes back as
// about 500 points whatever the window, so a wider one costs resolution. Quarter hours keep the
// sampling grid still from one poll to the next.
export function fetchPlatformExperimentTimeseries(
  pe: PlatformExperiment,
  metricName: string,
): Promise<{ series: AgentMetricSeries[] }> {
  const url = new URL(`${API_URL}/platform-experiments/${pe.id}/metrics-timeseries`)
  url.searchParams.set('metric_name', metricName)
  if (!isZeroDate(pe.starts_at)) {
    const end = Math.min(Date.now(), new Date(pe.ends_at as string).getTime())
    const quarters = Math.ceil((end - new Date(pe.starts_at as string).getTime()) / 900_000)
    url.searchParams.set('lookback_hours', String(Math.max(1, quarters) / 4))
    url.searchParams.set('end', new Date(pe.ends_at as string).toISOString())
  }
  return apiFetch<{ series: AgentMetricSeries[] }>(url.toString())
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

export function fetchHypothesesPage(params?: HypothesesParams): Promise<Page<HypothesisListItem>> {
  const url = new URL(`${API_URL}/hypotheses`)
  if (params) {
    for (const [k, v] of Object.entries(params)) {
      if (v !== undefined && v !== '') url.searchParams.set(k, String(v))
    }
  }
  return apiFetchPage<HypothesisListItem>(url.toString())
}

export const HYPOTHESIS_STATUSES: HypothesisStatus[] = ['open', 'confirmed', 'refuted', 'inconclusive']

export interface HypothesisStats {
  total: number
  /** A status no hypothesis holds is absent. */
  by_status: Partial<Record<HypothesisStatus, number>>
}

export function fetchHypothesisStats(platformExperimentID: string): Promise<HypothesisStats> {
  return apiFetch<HypothesisStats>(
    `${API_URL}/hypotheses/stats?platform_experiment_id=${encodeURIComponent(platformExperimentID)}`,
  )
}

// A hypothesis with a verdict, a finding or a job with a metric: its newest finding and the job
// testing it that did best on the ranking metric.
export interface HypothesisEvidence extends Hypothesis {
  finding?: HypothesisFinding
  best_job?: { id: string; metric: { name: string; best: number; basis: string } }
}

// Drawn from the 200 newest hypotheses.
export function fetchFindings(platformExperimentID: string): Promise<HypothesisEvidence[]> {
  return apiFetch<HypothesisEvidence[]>(`${API_URL}/platform-experiments/${platformExperimentID}/findings`)
}

export function fetchHypothesis(id: string): Promise<HypothesisWithJobs> {
  return apiFetch<HypothesisWithJobs>(`${API_URL}/hypotheses/${id}`)
}

export function fetchHypothesisGraph(platformExperimentID: string): Promise<HypothesisGraph> {
  return apiFetch<HypothesisGraph>(
    `${API_URL}/hypotheses/graph?platform_experiment_id=${encodeURIComponent(platformExperimentID)}`,
  )
}

export interface HypothesisQueryResult {
  columns: string[]
  rows: unknown[][]
  truncated: boolean
}

// Read-only Cypher against one platform experiment's hypothesis graph, which the backend binds as
// $pe. A Cypher error comes back as a 400 whose body is the database's own message.
export async function queryHypothesisGraph(platformExperimentID: string, cypher: string, params: Record<string, unknown>): Promise<HypothesisQueryResult> {
  const res = await fetch(`${API_URL}/hypotheses/query`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ platform_experiment_id: platformExperimentID, cypher, params }),
    cache: 'no-store',
  })
  if (!res.ok) throw new Error(await res.text())
  return res.json()
}

// The text index name and call syntax are defined by the backend in the POST /hypotheses/query
// operation description (served at /explore); keep this in step with it.
const HYPOTHESIS_SEARCH_CYPHER = `
CALL text_search.search_all('hypothesis_text', $q) YIELD node, score
WHERE node.platform_experiment_id = $pe
RETURN node.id AS id, node.text AS text, node.status AS status
ORDER BY score DESC
LIMIT 20`

export interface HypothesisSearchHit {
  id: string
  text: string
  status: HypothesisStatus
}

// The index parses its argument as a query: quotes, colons and brackets are syntax and uppercase
// AND/OR/NOT are operators, so typed text is passed on as lowercase words only. A literal /u regex
// needs an es6 target, which this tsconfig is not.
const NON_WORD = new RegExp('[^\\p{L}\\p{N}]+', 'gu')

export async function searchHypotheses(platformExperimentID: string, q: string): Promise<HypothesisSearchHit[]> {
  const words = q.toLowerCase().replace(NON_WORD, ' ').trim()
  if (!words) return []
  const result = await queryHypothesisGraph(platformExperimentID, HYPOTHESIS_SEARCH_CYPHER, { q: words })
  return result.rows.map(([id, text, status]) => ({
    id: id as string, text: text as string, status: status as HypothesisStatus,
  }))
}

export interface RegisteredHypothesis extends Hypothesis {
  already_existed: boolean
}

export interface SimilarHypothesis {
  id: string
  status: HypothesisStatus
  text: string
  reason: 'close_text' | 'refuted_parent'
}

/** A registration held back by a 409 until each listed hypothesis is told apart from it. */
export interface HeldBack {
  similar: SimilarHypothesis[]
}

// Registers a human's idea, placed in the graph by `links` (at least one builds_on; `to` is a
// hypothesis id, or the platform_experiment_id for the objective root). `author` is sent instead
// of an agent_id and there is no auth behind it. Text equivalent to an existing claim returns
// that row with already_existed set, and the supplied links are not applied. Text close to other
// hypotheses registers nothing and returns them until their ids are passed in distinctFrom.
export async function registerHumanHypothesis(
  platformExperimentID: string, author: string, text: string, links: HypothesisLinkInput[], distinctFrom: string[] = [],
): Promise<RegisteredHypothesis | HeldBack> {
  const res = await fetch(`${API_URL}/hypotheses`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ platform_experiment_id: platformExperimentID, author, text, links, distinct_from: distinctFrom }),
    cache: 'no-store',
  })
  if (res.status === 409) return { similar: (await res.json()).similar }
  if (!res.ok) throw new Error(await res.text())
  return res.json()
}

// Links are append-only. Only a human-authored hypothesis accepts links from a human.
export async function addHumanHypothesisLink(
  hypothesisID: string, author: string, type: HypothesisLinkType, to: string,
): Promise<void> {
  const res = await fetch(`${API_URL}/hypotheses/${hypothesisID}/links`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ author, type, to }),
    cache: 'no-store',
  })
  if (!res.ok) throw new Error(await res.text())
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
  max_agents: number
  /** Accelerators-in-flight (SUBMITTED+RUNNING) this experiment may hold at once. Omitted uses
   *  the platform default (quota.default_max_concurrent_accelerators). */
  max_concurrent_accelerators?: number
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
  /** "created_at" | "starts_at" | "name" | "status", optionally prefixed with "-" for descending. */
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

export function fetchPlatformExperiment(id: string): Promise<PlatformExperiment> {
  return apiFetch<PlatformExperiment>(`${API_URL}/platform-experiments/${id}`)
}

export interface AgentStanding {
  rank: number
  agent_id: string
  best: number
  experiment_id?: string
  hypothesis_id?: string
}

// Standings rank the competitors on each ranking metric, the first being the primary one. bests
// holds every reporting agent's best raw value on every declared metric and the job that set it.
export interface PlatformExperimentResults {
  metrics: { metric: string; direction: 'maximize' | 'minimize'; standings: AgentStanding[] }[]
  bests: {
    metric: string
    agents: { agent_id: string; best: number; experiment_id?: string }[]
    non_raw_basis_agents?: string[]
  }[]
}

export function fetchPlatformExperimentResults(id: string): Promise<PlatformExperimentResults> {
  return apiFetch<PlatformExperimentResults>(`${API_URL}/platform-experiments/${id}/results`)
}

// quotaTier optionally overrides this signup's guaranteed-vs-burst-only split regardless of the
// agent's kind — 'guaranteed' or 'burst_only'. Omit to use the kind default (humans
// guaranteed+burst, agents burst-only). Lets one experiment mix tiers across participants.
export type QuotaTierOverride = 'guaranteed' | 'burst_only' | undefined

export async function signupPlatformExperiment(
  id: string,
  agentID: string,
  quotaTier?: QuotaTierOverride,
): Promise<{ status: string }> {
  const res = await fetch(`${API_URL}/platform-experiments/${id}/signup`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ agent_id: agentID, ...(quotaTier ? { quota_tier: quotaTier } : {}) }),
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

// Resource pricing reference data (Accelerator type rates) — fetched from the backend instead of
// hardcoded, so the UI never drifts from the operator's config.
export interface ResourceCatalog {
  accelerator_types: Array<{ name: string; acch_rate: number }>
}


// Live, per-cluster accelerator capacity — what's actually schedulable right now, as opposed to
// fetchResourceCatalog's static type/rate list. Read from the metrics store
// (metricsdb.LiveClusterAcceleratorAvailableAndTotal).
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
