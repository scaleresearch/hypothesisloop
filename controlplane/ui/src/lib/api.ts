import type {
  AgentMessage,
  AgentSignup,
  APIKey,
  ClustersResponse,
  ClusterSettings,
  CreatedAPIKey,
  CreatedOrgInvite,
  JoinedAgent,
  Me,
  Org,
  OrgCluster,
  OrgInvite,
  OrgMember,
  OrgRole,
  Project,
  RegisteredCluster,
  Agent,
  Experiment,
  MetricDataPoint,
  PlatformExperiment,
  AgentQuota,
  MetricDefinition,
  AgentMetricSeries,
  Actor,
  Hypothesis,
  HypothesisComment,
  HypothesisFinding,
  HypothesisGraph,
  HypothesisLink,
  HypothesisLinkInput,
  HypothesisLinkType,
  HypothesisStatus,
  HypothesisWithJobs,
  JobMetric,
  GitHubApp,
  GitHubInstallation,
  GitHubRepository,
} from '@/types'

import type { TypeKind } from '@/lib/units'
import { accessToken, endSession } from '@/lib/auth'

// One API, one base URL: quota, scheduler and registry operations are all served together.
// Next.js inlines a NEXT_PUBLIC_ variable only where it is named in full.
export function apiURL(): string {
  const url = process.env.NEXT_PUBLIC_API_URL
  if (!url) throw new Error('NEXT_PUBLIC_API_URL is not set: copy .env.example to .env.local and fill it in')
  return url
}

/** A refusal or failure the API answered with: its status, its own message, and its reason code when it gave one. */
export class ApiError extends Error {
  /** `body` is the whole answer, for a refusal that carries more than its reason. */
  constructor(readonly status: number, message: string, readonly reason?: string, readonly body?: Record<string, unknown>) {
    super(message)
    this.name = 'ApiError'
  }
}

// The API answers a failure as {"error"} or {"reason","message"}.
async function failure(res: Response): Promise<ApiError> {
  const text = await res.text()
  let message = text
  let reason: string | undefined
  let body: Record<string, unknown> | undefined
  try {
    body = JSON.parse(text)
    message = (body?.message ?? body?.error ?? text) as string
    reason = body?.reason as string | undefined
  } catch {
    // A proxy in front of the API answers in its own format.
  }
  return new ApiError(res.status, message || `API error ${res.status}`, reason, body)
}

// Every call to the API goes through here: it carries the session's access token, and a session
// that is gone or that the API refuses has one outcome, the sign-in page.
async function request(method: string, path: string, body?: unknown): Promise<Response> {
  const token = await accessToken()
  if (!token) return endSession('ended')
  const res = await fetch(`${apiURL()}${path}`, {
    method,
    cache: 'no-store',
    headers: { Authorization: `Bearer ${token}`, ...(body === undefined ? {} : { 'Content-Type': 'application/json' }) },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  if (res.status === 401) return endSession('refused')
  return res
}

async function answer(method: string, path: string, body?: unknown): Promise<Response> {
  const res = await request(method, path, body)
  if (!res.ok) throw await failure(res)
  return res
}

export async function apiSend<T>(method: string, path: string, body?: unknown): Promise<T> {
  return (await answer(method, path, body)).json() as Promise<T>
}

function apiFetch<T>(path: string): Promise<T> {
  return apiSend<T>('GET', path)
}

// Query params are the fields of `params` sent under their own names, unset ones left out.
function withQuery(path: string, params: object): string {
  const query = new URLSearchParams()
  for (const [k, v] of Object.entries(params)) {
    if (v !== undefined && v !== '') query.set(k, String(v))
  }
  return query.size ? `${path}?${query}` : path
}

// A paginated list response: the page of items plus the total match count across all pages,
// read off the X-Total-Count response header, as every list endpoint sends it (?limit, ?offset,
// ?sort; total in X-Total-Count).
export interface Page<T> {
  items: T[]
  total: number
}

export const MAX_LIST_PAGE_SIZE = 200

async function apiFetchPage<T>(path: string): Promise<Page<T>> {
  const res = await answer('GET', path)
  const total = res.headers.get('X-Total-Count')
  if (total == null) throw new Error(`${path} answered a list without X-Total-Count`)
  return { items: (await res.json()) as T[], total: Number(total) }
}

// ---------------------------------------------------------------------------
// Cluster / Quota
// ---------------------------------------------------------------------------

// Every org's clusters with their live occupancy. Staff only: anyone else is answered 403.
export function fetchAllClusters(): Promise<ClustersResponse> {
  return apiFetch<ClustersResponse>(`/internal/clusters`)
}

// The clusters an org's jobs run on, its own and the ones the platform shares, and whether each
// one's cluster-agent is connected right now (has polled desired-state recently). The control
// plane never dials a cluster itself.
export function fetchOrgClusters(orgID: string): Promise<OrgCluster[]> {
  return apiFetch<{ clusters: OrgCluster[] }>(`/orgs/${orgID}/clusters`).then(r => r.clusters)
}

// Whether the org's jobs are placed on a cluster it can use, its own or a shared one. Turning it
// off leaves the jobs already there to finish.
export async function setClusterEnabled(orgID: string, name: string, enabled: boolean): Promise<void> {
  await apiSend('POST', `/orgs/${orgID}/clusters/${encodeURIComponent(name)}/${enabled ? 'enable' : 'disable'}`)
}

// The order the org's jobs are placed on the clusters it registered, every one of them named once,
// first placed first. The API writes the whole order or none of it, and answers the clusters.
export function setClusterOrder(orgID: string, names: string[]): Promise<OrgCluster[]> {
  return apiSend<{ clusters: OrgCluster[] }>('PUT', `/orgs/${orgID}/clusters/order`, { names }).then(r => r.clusters)
}

// Changes the fields named on a cluster the org registered: its settings, where 0 puts one back
// on the scheduler's default, and shared, which staff alone decide.
export async function updateCluster(orgID: string, name: string, change: ClusterSettings & { shared?: boolean }): Promise<void> {
  await apiSend('PATCH', `/orgs/${orgID}/clusters/${encodeURIComponent(name)}`, change)
}

// Ends a cluster: its jobs go back to the queue and its runtime token is refused from then on.
export async function removeCluster(orgID: string, name: string): Promise<void> {
  await apiSend('DELETE', `/orgs/${orgID}/clusters/${encodeURIComponent(name)}`)
}

// Registering a name the org already owns changes nothing and answers the same. The answer's
// runtime token is left behind here, so no page holds it.
export async function registerCluster(orgID: string, name: string): Promise<RegisteredCluster> {
  const registered = await apiSend<RegisteredCluster>('POST', `/orgs/${orgID}/clusters`, { name })
  return {
    name: registered.name, runtime_token: registered.runtime_token, attach_commands: registered.attach_commands,
    api_url: registered.api_url, metrics_url: registered.metrics_url, registry: registered.registry,
  }
}


// A list is asked for within a project. What is in a shared platform experiment of another org
// is asked for by platform_experiment_id alone: it lies in a project the caller's project_id
// never matches, and the API admits a list narrowed to a shared one.
export interface AgentsParams {
  project_id?: string
  platform_experiment_id?: string
  /** Substring match against id and name. */
  q?: string
  limit?: number
  offset?: number
}

export function fetchAgentsPage(params: AgentsParams): Promise<Page<Agent>> {
  return apiFetchPage<Agent>(withQuery('/agents', params))
}

// Whole-set figures for the filters fetchAgentsPage accepts, aggregated server-side.
export interface AgentStats {
  total: number
  top3_eligible: number
  avg_performance_score: number
}

export function fetchAgentStats(params: Pick<AgentsParams, 'project_id' | 'platform_experiment_id'>): Promise<AgentStats> {
  return apiFetch<AgentStats>(withQuery('/agents/stats', params))
}


// ---------------------------------------------------------------------------
// Registry / Experiments
// ---------------------------------------------------------------------------

// Field names are sent verbatim as query params, so they must match the API's own names.
export interface ExperimentsParams {
  project_id?: string
  status?: string
  agent_id?: string
  platform_experiment_id?: string
  /** Substring match against hypothesis and theory. */
  q?: string
  limit?: number
  offset?: number
  /** "created_at" | "updated_at" | "status", optionally prefixed with "-" for descending. */
  sort?: string
}

export function fetchExperimentsPage(params: ExperimentsParams): Promise<Page<Experiment>> {
  return apiFetchPage<Experiment>(withQuery('/experiments', params))
}

export function fetchExperiments(params: ExperimentsParams): Promise<Experiment[]> {
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
  // accelerator_count summed over SUBMITTED and RUNNING jobs: those on an accelerator type, and
  // those on CPU, whose count is cores.
  in_flight_accelerators: number
  in_flight_cores: number
  // Present only when group_by was asked for: the same stats per agent or hypothesis id.
  groups?: Record<string, ExperimentStats>
}

export function fetchExperimentStats(
  params: Omit<ExperimentsParams, 'limit' | 'offset' | 'sort'> & { group_by?: 'agent' | 'hypothesis' },
): Promise<ExperimentStats> {
  return apiFetch<ExperimentStats>(withQuery('/experiments/stats', params))
}

// The same counts over every org's jobs. Staff only: anyone else gets their own orgs' jobs.
export function fetchAllExperimentStats(): Promise<ExperimentStats> {
  return apiFetch<ExperimentStats>(`/experiments/stats`)
}

export function fetchExperiment(id: string): Promise<Experiment> {
  return apiFetch<Experiment>(`/experiments/${id}`)
}


export function fetchExperimentMetrics(id: string): Promise<MetricDataPoint[]> {
  return apiFetch<MetricDataPoint[]>(`/experiments/${id}/metrics`)
}

// The job's most recently reported stdout/stderr tail. For a failed job this is usually the
// only place the error itself appears — the record carries a status and a phase_detail reason,
// but the traceback lives here.
export function fetchExperimentLogs(id: string, n = 200): Promise<string[]> {
  return apiFetch<string[]>(`/experiments/${id}/logs?n=${n}`)
}

// Metric history of every job in a platform experiment over its own run, one series per job. The
// window reaches from the run's start to its end, or to now while it runs: each series comes back as
// about 500 points whatever the window, so a wider one costs resolution. Quarter hours keep the
// sampling grid still from one poll to the next.
export function fetchPlatformExperimentTimeseries(
  pe: PlatformExperiment,
  metricName: string,
): Promise<{ series: AgentMetricSeries[] }> {
  const query = new URLSearchParams({ metric_name: metricName })
  if (pe.started_at) {
    const end = Math.min(Date.now(), new Date(pe.ends_at as string).getTime())
    const quarters = Math.ceil((end - new Date(pe.started_at).getTime()) / 900_000)
    query.set('lookback_hours', String(Math.max(1, quarters) / 4))
    query.set('end', new Date(pe.ends_at as string).toISOString())
  }
  return apiFetch<{ series: AgentMetricSeries[] }>(`/platform-experiments/${pe.id}/metrics-timeseries?${query}`)
}

// ---------------------------------------------------------------------------
// Hypotheses
// ---------------------------------------------------------------------------

export const HYPOTHESIS_STATUSES: HypothesisStatus[] = ['open', 'confirmed', 'refuted', 'inconclusive']

export interface HypothesisStats {
  total: number
  /** A status no hypothesis holds is absent. */
  by_status: Partial<Record<HypothesisStatus, number>>
}

export function fetchHypothesisStats(platformExperimentID: string): Promise<HypothesisStats> {
  return apiFetch<HypothesisStats>(
    `/hypotheses/stats?platform_experiment_id=${encodeURIComponent(platformExperimentID)}`,
  )
}

// A hypothesis with a verdict, a finding or a job with a metric: its newest finding and the job
// testing it that did best on the ranking metric.
export interface HypothesisEvidence extends Hypothesis {
  finding?: HypothesisFinding
  // summary is the write-up filed for that job, absent when none was.
  best_job?: { id: string; metric: JobMetric; summary?: string }
}

// Drawn from the 200 newest hypotheses.
export function fetchFindings(platformExperimentID: string): Promise<HypothesisEvidence[]> {
  return apiFetch<HypothesisEvidence[]>(`/platform-experiments/${platformExperimentID}/findings`)
}

export function fetchHypothesis(id: string): Promise<HypothesisWithJobs> {
  return apiFetch<HypothesisWithJobs>(`/hypotheses/${id}`)
}

export function fetchHypothesisGraph(platformExperimentID: string): Promise<HypothesisGraph> {
  return apiFetch<HypothesisGraph>(
    `/hypotheses/graph?platform_experiment_id=${encodeURIComponent(platformExperimentID)}`,
  )
}

interface HypothesisQueryResult {
  columns: string[]
  rows: unknown[][]
  truncated: boolean
}

// Read-only Cypher against one platform experiment's hypothesis graph, which the backend binds as
// $pe. A Cypher error comes back as a 400 whose body is the database's own message.
function queryHypothesisGraph(platformExperimentID: string, cypher: string, params: Record<string, unknown>): Promise<HypothesisQueryResult> {
  return apiSend<HypothesisQueryResult>('POST', '/hypotheses/query', { platform_experiment_id: platformExperimentID, cypher, params })
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
  /** Its outgoing links as stored. */
  links: HypothesisLink[]
  /** The hypotheses it reads close to, or the refuted ones it builds on, as a hint to the person. */
  similar: SimilarHypothesis[]
}

export interface SimilarHypothesis {
  id: string
  agent_id?: string
  actor: Actor
  builds_on: string[]
  status: HypothesisStatus
  text: string
  reason: 'close_text' | 'refuted_parent'
}

// Registers a human's idea, placed in the graph by `links` (at least one builds_on; `to` is a
// hypothesis id, or the platform_experiment_id for the objective root). Who registered it is the
// signed-in person, read by the API from their session. Text equivalent to an existing claim returns
// that row with already_existed set, and the supplied links are not applied. Text close to other
// hypotheses is registered all the same, and those come back in similar.
export function registerHumanHypothesis(
  platformExperimentID: string, text: string, links: HypothesisLinkInput[],
): Promise<RegisteredHypothesis> {
  return apiSend<RegisteredHypothesis>('POST', '/hypotheses', { platform_experiment_id: platformExperimentID, text, links })
}

// Links are append-only. Only a hypothesis no agent owns accepts links from a person as themselves.
// Answers the hypothesis's outgoing links.
export function addHumanHypothesisLink(
  hypothesisID: string, type: HypothesisLinkType, to: string,
): Promise<HypothesisLink[]> {
  return apiSend<HypothesisLink[]>('POST', `/hypotheses/${hypothesisID}/links`, { type, to })
}

// The author edits their own idea, and the author or an org owner removes it, while no agent has
// acted on it and the experiment is open.
export function editHumanHypothesis(hypothesisID: string, text: string): Promise<Hypothesis> {
  return apiSend<Hypothesis>('PATCH', `/hypotheses/${hypothesisID}`, { text })
}

export async function removeHumanHypothesis(hypothesisID: string): Promise<void> {
  await answer('DELETE', `/hypotheses/${hypothesisID}`)
}

export function addHumanHypothesisComment(hypothesisID: string, text: string): Promise<HypothesisComment> {
  return apiSend<HypothesisComment>('POST', `/hypotheses/${hypothesisID}/comments`, { text })
}

export async function cancelExperiment(id: string): Promise<void> {
  await apiSend('POST', `/experiments/${id}/cancel`)
}

// ---------------------------------------------------------------------------
// Platform Experiments
// ---------------------------------------------------------------------------

export interface CreatePlatformExperimentRequest {
  name: string
  description: string
  budget_accelerator_hours: number
  /** Omitted is as many as the platform runs machines for one experiment. */
  max_agents?: number
  /** Accelerators-in-flight (SUBMITTED+RUNNING) this experiment may hold at once. Omitted uses
   *  the platform default (quota.default_max_concurrent_accelerators). */
  max_concurrent_accelerators?: number
  metrics: MetricDefinition[]
  /** Omitted is the platform's default. */
  report_interval_seconds?: number
  /** Elimination ladder; omitted means the platform default. */
  stages?: Stage[]
  /** When its baseline begins to be measured. Omitted, the create. */
  starts_at?: string
  /** How long the run lasts from its start, once the baseline is declared. */
  duration_hours: number
  /** owner/name of a repository a connected GitHub account covers. Read at creation only. */
  repository?: string
  /** The image hosted agents and the coordinator run; omitted means the platform's. Read at creation only. */
  agent_image?: string
  /** true has the platform run the coordinator. It needs `repository`. Read at creation only. */
  coordinator_hosted?: boolean
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

/** The reason the API gives for a repository that brings no experiment file. */
export const NO_EXPERIMENT_FILE = 'no_experiment_file'

export function fetchExperimentFile(projectID: string, repository: string): Promise<ExperimentFile> {
  return apiFetch<ExperimentFile>(withQuery(`/projects/${projectID}/experiment-file`, { repository }))
}

export function createPlatformExperiment(projectID: string, req: CreatePlatformExperimentRequest): Promise<PlatformExperiment> {
  return apiSend<PlatformExperiment>('POST', '/platform-experiments', { ...req, project_id: projectID })
}

export interface PlatformExperimentsParams {
  /** The project's own. The ones staff shared with every account are listed with them, in every project. */
  project_id?: string
  /** true keeps only the shared ones, false leaves them out. */
  shared?: boolean
  status?: string
  /** Search over name/description. */
  q?: string
  limit?: number
  offset?: number
  /** "created_at" | "starts_at" | "name" | "status", optionally prefixed with "-" for descending. */
  sort?: string
}

export function fetchPlatformExperimentsPage(params: PlatformExperimentsParams): Promise<Page<PlatformExperiment>> {
  return apiFetchPage<PlatformExperiment>(withQuery('/platform-experiments', params))
}

export function fetchPlatformExperiment(id: string): Promise<PlatformExperiment> {
  return apiFetch<PlatformExperiment>(`/platform-experiments/${id}`)
}

interface AgentStanding {
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
  metrics: { metric: string; direction: 'maximize' | 'minimize'; standings: AgentStanding[] }[]
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

export function fetchPlatformExperimentResults(id: string): Promise<PlatformExperimentResults> {
  return apiFetch<PlatformExperimentResults>(`/platform-experiments/${id}/results`)
}

export function fetchPlatformExperimentQuotas(platformExpID: string): Promise<AgentQuota[]> {
  return apiFetch<AgentQuota[]>(`/platform-experiments/${platformExpID}/quotas`)
}

// Answered 404 for an agent with no quota there: quota is granted at the start, to the agents signed up.
export function fetchAgentQuota(platformExpID: string, agentID: string): Promise<AgentQuota> {
  return apiFetch<AgentQuota>(`/platform-experiments/${platformExpID}/quotas/${agentID}`)
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

// What the jobs of one org can be placed on: its own clusters and the ones the platform shares.
export function fetchResourceCapacity(orgID: string): Promise<ResourceCapacity> {
  return apiFetch<ResourceCapacity>(withQuery('/resource-catalog/capacity', { org_id: orgID }))
}

/** The fields of a platform experiment that can change after it is created. */
export type PlatformExperimentPatch = Partial<Pick<CreatePlatformExperimentRequest,
  'name' | 'description' | 'budget_accelerator_hours' | 'max_agents' | 'max_concurrent_accelerators' | 'metrics' |
  'report_interval_seconds' | 'starts_at' | 'duration_hours'>>

/** Changes the fields the patch names and no others. */
export function updatePlatformExperiment(id: string, patch: PlatformExperimentPatch): Promise<PlatformExperiment> {
  return apiSend<PlatformExperiment>('PATCH', `/platform-experiments/${id}`, patch)
}

export interface Stage {
  length_pct: number
  evict_pct: number
  /** Longest a single job may run while this stage is current. 0/absent = unlimited. */
  max_job_hours?: number
}

interface AgentCut {
  agent_id: string
  stage_index: number
}

interface StageAdvance {
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

export function fetchStages(platformExpID: string): Promise<StagesStatus> {
  return apiFetch<StagesStatus>(`/platform-experiments/${platformExpID}/stages`)
}

// ---------------------------------------------------------------------------
// Agents of a platform experiment
// ---------------------------------------------------------------------------

export function fetchPlatformExperimentAgents(platformExpID: string): Promise<JoinedAgent[]> {
  return apiFetch<{ agents: JoinedAgent[] }>(`/platform-experiments/${platformExpID}/agents`).then(r => r.agents)
}

export interface AddAgentsRequest {
  /** true: the platform runs the agent. false: its token comes back, to run it yourself. */
  hosted: boolean
  flavor?: string
  model?: string
}

// An agent the platform does not run comes back with its token.
export function addPlatformExperimentAgents(platformExpID: string, req: AddAgentsRequest): Promise<JoinedAgent[]> {
  return apiSend<{ agents: JoinedAgent[] }>('POST', `/platform-experiments/${platformExpID}/agents`, req).then(r => r.agents)
}

// The token of an agent that already joined: joining again changes nothing and answers with the
// same token, which is derived from the agent's place in the experiment.
export async function fetchPlatformExperimentAgentToken(platformExpID: string, agentID: string): Promise<JoinedAgent> {
  const { agents } = await apiSend<{ agents: JoinedAgent[] }>('POST', `/platform-experiments/${platformExpID}/agents`, { agent_id: agentID })
  if (agents.length !== 1) throw new Error(`asked for the token of agent ${agentID} and was answered ${agents.length} agents`)
  return agents[0]
}

// Every message the coordinator and the owner sent to an agent of the experiment, oldest first.
export function fetchPlatformExperimentMessages(platformExpID: string): Promise<AgentMessage[]> {
  return apiFetch<{ messages: AgentMessage[] }>(`/platform-experiments/${platformExpID}/messages`).then(r => r.messages)
}

// The agent is woken with it at once. Answered 429 past the agent's hourly limit.
export async function sendPlatformExperimentMessage(platformExpID: string, agentID: string, text: string): Promise<AgentMessage[]> {
  const { messages } = await apiSend<{ messages: AgentMessage[] }>('POST', `/platform-experiments/${platformExpID}/messages`, { agent_id: agentID, text })
  return messages
}

// Answered 409 once the platform experiment has started.
export async function removePlatformExperimentAgent(platformExpID: string, agentID: string): Promise<void> {
  await apiSend('DELETE', `/platform-experiments/${platformExpID}/agents/${agentID}`)
}

// ---------------------------------------------------------------------------
// Accounts
// ---------------------------------------------------------------------------

export function fetchMe(): Promise<Me> {
  return apiFetch<Me>(`/me`)
}

// The new org starts with the caller as owner and a project named Default project.
export function createOrg(name: string): Promise<Org> {
  return apiSend<Org>('POST', '/orgs', { name })
}

export function renameOrg(orgID: string, name: string): Promise<Org> {
  return apiSend<Org>('PATCH', `/orgs/${orgID}`, { name })
}

// A name another project of the org carries is answered 409.
export function createProject(orgID: string, name: string): Promise<Project> {
  return apiSend<Project>('POST', `/orgs/${orgID}/projects`, { name })
}

export function renameProject(projectID: string, name: string): Promise<Project> {
  return apiSend<Project>('PATCH', `/projects/${projectID}`, { name })
}

// Newest first, revoked ones included. No token is listed.
export function fetchAPIKeys(orgID: string): Promise<APIKey[]> {
  return apiFetch<{ api_keys: APIKey[] }>(`/orgs/${orgID}/api-keys`).then(r => r.api_keys)
}

// The answer is the only place the key's token ever appears. Without a project the key reaches
// the whole org.
export function createAPIKey(orgID: string, name: string, projectID?: string): Promise<CreatedAPIKey> {
  return apiSend<CreatedAPIKey>('POST', `/orgs/${orgID}/api-keys`, { name, project_id: projectID })
}

export function revokeAPIKey(keyID: string): Promise<APIKey> {
  return apiSend<APIKey>('DELETE', `/api-keys/${keyID}`)
}

// Oldest first. A member is named by user id alone.
export function fetchOrgMembers(orgID: string): Promise<Page<OrgMember>> {
  return apiFetchPage<OrgMember>(`/orgs/${orgID}/members?limit=${MAX_LIST_PAGE_SIZE}`)
}

// Demoting the last owner is answered 409 with reason last_owner.
export function setOrgMemberRole(orgID: string, userID: string, role: OrgRole): Promise<OrgMember> {
  return apiSend<OrgMember>('PATCH', `/orgs/${orgID}/members/${userID}`, { role })
}

// For an owner, and for a member naming themselves. The last owner is answered 409 with reason last_owner.
export async function removeOrgMember(orgID: string, userID: string): Promise<void> {
  await answer('DELETE', `/orgs/${orgID}/members/${userID}`)
}

// The invites that can still be accepted, newest first. No token is listed.
export function fetchOrgInvites(orgID: string): Promise<Page<OrgInvite>> {
  return apiFetchPage<OrgInvite>(`/orgs/${orgID}/invites?limit=${MAX_LIST_PAGE_SIZE}`)
}

// The answer is the only place the invite's token ever appears.
export function createOrgInvite(orgID: string, role: OrgRole): Promise<CreatedOrgInvite> {
  return apiSend<CreatedOrgInvite>('POST', `/orgs/${orgID}/invites`, { role })
}

export async function revokeOrgInvite(inviteID: string): Promise<void> {
  await answer('DELETE', `/invites/${inviteID}`)
}

// The signed-in person joins the invite's org with the invite's role. A token that cannot be
// accepted is answered 409 with the reason, one the platform does not know 404.
export function acceptOrgInvite(token: string): Promise<OrgMember> {
  return apiSend<OrgMember>('POST', '/invites/accept', { token })
}

// ---------------------------------------------------------------------------
// GitHub
// ---------------------------------------------------------------------------

/** The reason the API gives on a deployment whose git host is not GitHub. */
export const GITHUB_NOT_CONFIGURED = 'github_not_configured'
/** The person's GitHub account reaches no installation of the App. */
export const APP_NOT_INSTALLED = 'app_not_installed'
/** The person reaches several installations; the refusal's body lists them as `installations`. */
export const INSTALLATION_CHOICE_REQUIRED = 'installation_choice_required'

// Answered 503 github_not_configured on a deployment whose git host is not GitHub.
export function fetchGitHubApp(): Promise<GitHubApp> {
  return apiFetch<GitHubApp>('/github/app')
}

// Oldest first, as recorded at connect: GitHub is not asked.
export function fetchGitHubInstallations(orgID: string): Promise<Page<GitHubInstallation>> {
  return apiFetchPage<GitHubInstallation>(`/orgs/${orgID}/github-installations?limit=${MAX_LIST_PAGE_SIZE}`)
}

// `code` is what GitHub returned from authorising the App, to `redirectURI`: it proves the person
// reaches the installation, and works once. Without `installationID` the one installation they
// reach is connected, and reaching several is refused with INSTALLATION_CHOICE_REQUIRED.
export function connectGitHubInstallation(
  orgID: string, connect: { code: string; redirectURI: string; installationID?: number },
): Promise<GitHubInstallation> {
  return apiSend<GitHubInstallation>('POST', `/orgs/${orgID}/github-installations`, {
    code: connect.code, redirect_uri: connect.redirectURI, installation_id: connect.installationID,
  })
}

export async function disconnectGitHubInstallation(orgID: string, installationID: number): Promise<void> {
  await answer('DELETE', `/orgs/${orgID}/github-installations/${installationID}`)
}

// Read from GitHub on every call, so a caller that searches as the person types waits for a pause.
export function fetchGitHubRepositories(orgID: string, params: { q?: string; limit?: number } = {}): Promise<Page<GitHubRepository>> {
  return apiFetchPage<GitHubRepository>(withQuery(`/orgs/${orgID}/github-repositories`, params))
}
