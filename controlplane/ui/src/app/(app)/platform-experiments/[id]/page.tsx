'use client'

import { Fragment, useState } from 'react'
import { useProjectRouter } from '@/lib/query-state'
import { isReadOnly, ReadOnlyProvider, useRecordScope } from '@/lib/read-only'
import { useTenant } from '@/lib/tenant'
import { SharedBadge } from '@/components/shared-badge'
import { TextLink } from '@/components/ui/link'
import useSWR from 'swr'
import { clsx } from 'clsx'
import {
  fetchPlatformExperiment,
  fetchPlatformExperimentQuotas,
  fetchPlatformExperimentResults,
  fetchExperimentsPage,
  fetchExperimentStats,
  fetchStages,
  fetchPlatformExperimentTimeseries,
  fetchHypothesisStats,
  fetchFindings,
  HYPOTHESIS_STATUSES,
} from '@/lib/api'
import type { StagesStatus } from '@/lib/api'
import type { PlatformExperiment, AgentQuota, Experiment, AgentMetricSeries } from '@/types'
import { PlatformExperimentStatus } from '@/types'
import { Baseline } from '@/components/baseline'
import { JobHypothesis } from '@/components/job-hypothesis'
import { DraftView } from '@/components/draft'
import { AgentJobsPanel, AgentSelector, CompetingAgentsChart, LiveMetricsPicker, MetricSelector, useAgentColorMap } from '@/components/experiment-charts'
import { ExperimentAgents } from '@/components/experiment-agents'
import { HostedMachines } from '@/components/hosted-machines'
import { AgentLabel, AgentNames } from '@/components/agent-label'
import { Actor, verdictBy } from '@/components/actor'
import { Card, CardHeader, CardContent, CardTitle } from '@/components/ui/card'
import { Badge, NoSummary } from '@/components/ui/badge'
import { MetricBar } from '@/components/ui/metric-bar'
import { CollapsibleDescription } from '@/components/ui/collapsible-description'
import { StatTile } from '@/components/ui/stat-tile'
import { PageHeader } from '@/components/ui/page-header'
import { ExperimentCode } from '@/components/experiment-code'
import { Loading, ErrorMessage, EmptyState } from '@/components/ui/status-message'
import { LinkTr, Table, num } from '@/components/ui/table'
import { Eyebrow, Pill } from '@/components/ui/text'
import { semantic, chart } from '@/lib/colors'
import { formatAccH, formatDate, runClock } from '@/lib/format'
import { evictionLabel, faultBreakdown } from '@/lib/eviction'
import { JobReason } from '@/components/job-reason'
import { filedAfterVerdict, hypothesisActivity, settings, variedEnvByHypothesis, variedEnvSettings, variedKeys } from '@/lib/hypothesis-progress'
import { graphHref } from '@/lib/hypothesis-graph'
import { budgetKind, budgetUnit, inFlight, kindOf } from '@/lib/units'
import type { Draft } from '@/lib/planning'

function StatusBadge({ status }: { status: string }) {
  return <Badge status={status}>{status}</Badge>
}

const CLOSED_BY_THE_PLATFORM: Partial<Record<NonNullable<PlatformExperiment['close_reason']>, string>> = {
  end_time_reached: 'reached its end time',
  planning_limit_reached: 'never ran: its planner proposed nothing within the planning limit',
  proposal_expired: 'never ran: nobody took up its planner\'s answer in time',
  baseline_limit_reached: 'never ran: no baseline was measured and declared in time',
}

/** Why a closed experiment closed. Nothing for one closed before reasons were kept. */
function CloseReason({ pe }: { pe: PlatformExperiment }) {
  const byThePlatform = pe.close_reason && CLOSED_BY_THE_PLATFORM[pe.close_reason]
  if (byThePlatform) return <span className="text-xs text-subtle">{byThePlatform}</span>
  if (pe.close_reason !== 'closed_by_request' || !pe.closed_by) return null
  return (
    <span className="inline-flex items-center gap-1.5 text-xs text-subtle" title={pe.closed_at && formatDate(pe.closed_at)}>
      closed early by <Actor actor={pe.closed_by} />
    </span>
  )
}

// How many of the newest jobs the page reads, for the jobs table and the metrics picker.
const RECENT_JOBS = 20

export default function PlatformExperimentDetailPage({ params }: { params: { id: string } }) {
  return <AgentNames platformExperimentID={params.id}><PlatformExperimentDetail params={params} /></AgentNames>
}

function PlatformExperimentDetail({ params }: { params: { id: string } }) {
  const { id } = params
  const router = useProjectRouter()
  const [selectedAgentIds, setSelectedAgentIds] = useState<Set<string> | null>(null)
  const [selectedMetricKeys, setSelectedMetricKeys] = useState<Set<string> | null>(null)
  const [expandedAgentId, setExpandedAgentId] = useState<string | null>(null)

  const { data: pe, error: peError, isLoading: peLoading } = useSWR<PlatformExperiment>(
    ['pe', id],
    () => fetchPlatformExperiment(id),
    { refreshInterval: 15_000 },
  )
  const { me } = useTenant()
  useRecordScope(pe?.project_id)

  const { data: quotas, error: quotasError } = useSWR<AgentQuota[]>(
    ['pe-quotas', id],
    () => fetchPlatformExperimentQuotas(id),
    { refreshInterval: 15_000 },
  )

  // Each list below is asked for inside the platform experiment's own project, so it waits for
  // the experiment itself. The newest jobs only, for the jobs table and the live metrics picker.
  const { data: experimentsPage, error: experimentsError } = useSWR(
    pe ? ['pe-experiments', id] : null,
    () => fetchExperimentsPage({ project_id: pe!.project_id, platform_experiment_id: id, sort: '-created_at', limit: RECENT_JOBS }),
    { refreshInterval: 15_000 },
  )
  const experiments = experimentsPage?.items

  // Asked for by status: a job that outlived the newest page of jobs is still running.
  const { data: runningPage, error: runningError } = useSWR(
    pe ? ['pe-running', id] : null,
    () => fetchExperimentsPage({ project_id: pe!.project_id, platform_experiment_id: id, status: 'RUNNING', sort: '-created_at' }),
    { refreshInterval: 15_000 },
  )

  // Whole-experiment job counts, grouped server-side. The job list above is one bounded page,
  // so tallying it would report the page's shape under a label that reads as the experiment's.
  const { data: stats, error: statsError } = useSWR(
    pe ? ['pe-stats-by-agent', id] : null,
    () => fetchExperimentStats({ project_id: pe!.project_id, platform_experiment_id: id, group_by: 'agent' }),
    { refreshInterval: 15_000 },
  )
  const jobsByAgent = stats?.groups ?? {}
  const { data: hypothesisStats, error: hypothesisStatsError } = useSWR(
    pe ? ['pe-stats-by-hypothesis', id] : null,
    () => fetchExperimentStats({ project_id: pe!.project_id, platform_experiment_id: id, group_by: 'hypothesis' }),
    { refreshInterval: 15_000 },
  )

  const { data: stagesStatus, error: stagesError } = useSWR<StagesStatus>(
    ['pe-stages', id],
    () => fetchStages(id),
    { refreshInterval: 15_000 },
  )

  const { data: verdicts, error: hypothesesError } = useSWR(
    ['pe-hypothesis-stats', id],
    () => fetchHypothesisStats(id),
    { refreshInterval: 15_000 },
  )

  const { data: findings, error: findingsError } = useSWR(
    ['pe-findings', id],
    () => fetchFindings(id),
    { refreshInterval: 15_000 },
  )

  const { data: results, error: resultsError } = useSWR(
    ['pe-results', id],
    () => fetchPlatformExperimentResults(id),
    { refreshInterval: 15_000 },
  )

  // The history of every declared metric, for the chart. The heaviest read on the page, so it
  // polls slower than the rest.
  const peMetricKeys = (pe?.metrics ?? []).map(m => m.key)
  const { data: allMetricSeries, error: metricSeriesError } = useSWR<{ key: string; series: AgentMetricSeries[] }[]>(
    pe && peMetricKeys.length > 0 ? ['pe-all-metrics', id, peMetricKeys.join(','), pe.started_at, pe.ends_at] : null,
    () => Promise.all(peMetricKeys.map(k =>
      fetchPlatformExperimentTimeseries(pe!, k).then(r => ({ key: k, series: r.series })),
    )),
    { refreshInterval: 30_000 },
  )

  // Every agent known to this platform experiment, whether via quota signup or a submitted job.
  // Computed unconditionally (before the loading/not-found early returns below) so hook order
  // stays stable across renders — useAgentColorMap can't follow a conditional return.
  const knownAgentIds = Array.from(new Set([
    ...(quotas ?? []).map(q => q.agent_id),
    ...Object.keys(jobsByAgent),
  ])).sort()
  const colorMap = useAgentColorMap(knownAgentIds)
  const colorFor = (agentId: string) => colorMap.get(agentId) ?? semantic.neutral

  // Every number below is derived by joining these reads together, so a failed one must not
  // pass through as an empty array: an unreachable quota service would render as "no agents
  // signed up", which is a different answer, not a missing one.
  const failed = [
    quotasError && 'quotas',
    (experimentsError || runningError) && 'jobs',
    stagesError && 'stage status',
    (hypothesesError || findingsError) && 'hypotheses',
    metricSeriesError && 'metric history',
    resultsError && 'results',
    (statsError || hypothesisStatsError) && 'job counts',
  ].filter(Boolean) as string[]

  if (peLoading) return <Loading />
  if (peError) return <ErrorMessage>Could not load this platform experiment: {peError.message}</ErrorMessage>
  if (pe?.status === PlatformExperimentStatus.DRAFT) return <DraftView draft={pe} readOnly={isReadOnly(me, pe)} />
  if (failed.length > 0) return <ErrorMessage>Could not load {failed.join(', ')} for this platform experiment.</ErrorMessage>
  if (!pe) return <ErrorMessage>Experiment not found.</ErrorMessage>

  const trackedMetricKeys = (pe.metrics ?? []).map((m) => m.key)
  const metrics = pe.metrics ?? []

  // Ordered as the server ranks the competitors on the primary ranking metric; an agent without a
  // standing (not a competitor, ineligible, or nothing reported) follows, by id.
  const primaryStandings = results?.metrics[0]
  const standingOf = new Map((primaryStandings?.standings ?? []).map(st => [st.agent_id, st]))
  const unranked = Number.MAX_SAFE_INTEGER
  const scoreboard = [...knownAgentIds].sort((a, b) => (standingOf.get(a)?.rank ?? unranked) - (standingOf.get(b)?.rank ?? unranked))
  // What tells the agents' best jobs apart; a lone agent's best job shows every setting.
  const standingEnvs = (primaryStandings?.standings ?? []).map(st => st.env)
  const standingKeys = standingEnvs.length > 1 ? variedKeys(standingEnvs) : Object.keys(standingEnvs[0] ?? {}).sort()
  const hypothesisText = new Map((findings ?? []).map(h => [h.id, h.text]))
  const bestOf = (metricKey: string, agentId: string) =>
    results?.bests.find(b => b.metric === metricKey)?.agents.find(a => a.agent_id === agentId)
  const totalUsed = (quotas ?? []).reduce((s, q) => s + q.used_guaranteed_acch + q.used_burst_acch, 0)
  const hypothesisCount = verdicts?.total ?? 0
  const hypothesisSub = [
    HYPOTHESIS_STATUSES.map(v => `${verdicts?.by_status[v] ?? 0} ${v}`).join(' · '),
    hypothesisActivity(hypothesisCount, hypothesisStats?.groups ?? {}, id),
  ].filter(Boolean).join(' · ')

  // Best result first; a hypothesis whose jobs reported no metric comes last.
  const bestFirst = primaryStandings?.direction === 'minimize' ? 1 : -1
  const found = [...(findings ?? [])].sort((a, b) =>
    (a.best_job ? bestFirst * a.best_job.metric.value : Infinity) - (b.best_job ? bestFirst * b.best_job.metric.value : Infinity))

  // SUBMITTED and RUNNING, the set max_concurrent_accelerators is checked against.
  const held = inFlight(stats?.in_flight_accelerators ?? 0, stats?.in_flight_cores ?? 0, pe.max_concurrent_accelerators)
  const unit = budgetUnit(budgetKind((pe as Draft).proposal?.accelerator_type, (experiments ?? []).map(exp => exp.accelerator_type)))
  const clock = runClock(pe)

  // Running experiments
  const jobsByStatus = stats?.by_status ?? {}
  // One list: what is running first, then the newest of the rest.
  const running = runningPage?.items ?? []
  const jobRows = [...running, ...(experiments ?? []).filter(exp => !running.some(r => r.id === exp.id))]
  const variedByHypothesis = variedEnvByHypothesis(jobRows)

  // Evicted jobs, broken down by eviction_reason — a platform experiment can evict for very
  // different reasons (stuck on admission vs. silent vs. ran past a stage's job
  // length cap), so a single opaque count hides whether evictions are a symptom of something
  // wrong versus routine stage cuts. Counted server-side over the whole experiment, not over
  // the page of jobs above it, and already grouped by code (a reason may carry a ': detail'
  // explanation — see lib/eviction).
  const evictionBreakdown = Object.entries(stats?.evictions_by_reason ?? {})
    .sort((a, b) => b[1] - a[1])
    .map(([r, n]) => `${evictionLabel(r)} (${n})`)
    .join(' · ')

  // Whose fault those evictions were, folded from the same tally — an infrastructure count here
  // says the platform failed these jobs, not the agents running them.
  const evictionFaultBreakdown = faultBreakdown(stats?.evictions_by_class ?? {})

  // Default both selectors to "everything" until the user narrows them down.
  const activeAgentIds = selectedAgentIds ?? new Set(knownAgentIds)
  const activeMetricKeys = selectedMetricKeys ?? new Set(metrics.map(m => m.key))

  const readOnly = isReadOnly(me, pe)

  return (
    <ReadOnlyProvider value={readOnly}>
    <div>
      {/* Header */}
      <PageHeader
        back={{ label: 'Experiments', onClick: () => router.push('/platform-experiments') }}
        title={pe.name}
        description={
          <div className="flex flex-wrap items-center gap-2.5">
            <StatusBadge status={pe.status} />
            <CloseReason pe={pe} />
            <SharedBadge pe={pe} />
            {clock && <span className="text-xs text-subtle">{clock}</span>}
            <TextLink href={graphHref(pe.id)} className="text-xs">
              {hypothesisCount} Hypotheses →
            </TextLink>
            <div className="basis-full">
              <ExperimentCode pe={pe} />
            </div>
          </div>
        }
      />

      <HostedMachines pe={pe} jobs={stats?.total ?? 0} />
      <Baseline pe={pe} />

      {readOnly && (
        <p role="note" className="-mt-3 mb-4 text-xs text-muted">
          The platform shares this experiment with every account. You can read everything in it and change nothing.
        </p>
      )}

      {/* Description + metrics */}
      {(pe.description || (pe.metrics && pe.metrics.length > 0)) && (
        <Card>
          <CardHeader><CardTitle>About this experiment</CardTitle></CardHeader>
          <CardContent>
            {pe.description && (
              <CollapsibleDescription text={pe.description} className={pe.metrics?.length ? 'mb-3' : undefined} />
            )}
            {pe.metrics && pe.metrics.length > 0 && (
              <div>
                <Eyebrow className="mb-1.5 block">Optimization Metrics</Eyebrow>
                <div className="flex flex-wrap gap-2">
                  {pe.metrics.map((m, i) => (
                    <Pill key={m.key} active={i === 0}>
                      {i === 0 && <span className="font-bold">PRIMARY</span>}
                      {m.key} {m.direction === 'maximize' ? '↑ maximize' : '↓ minimize'}
                    </Pill>
                  ))}
                </div>
              </div>
            )}
          </CardContent>
        </Card>
      )}

      {/* Findings */}
      {found.length > 0 && (
        <Card>
          <CardHeader><CardTitle note={`${found.length} hypotheses with a verdict or a result`}>Findings</CardTitle></CardHeader>
          <CardContent scrollX>
            <Table className="min-w-[760px] table-fixed">
              <thead>
                <tr>
                  <th className="w-[110px]">Verdict</th>
                  <th>Hypothesis</th>
                  <th className="w-[170px] text-right">Best job</th>
                  <th title="The best job's own summary. Without one, the newest finding of the hypothesis, named with the job it is about.">Finding</th>
                </tr>
              </thead>
              <tbody>
                {found.map(h => (
                  <LinkTr key={h.id} href={`/hypotheses/${h.id}`} className="[&>td]:align-top">
                    <td>
                      <Badge status={h.status}>{h.status}</Badge>
                      <div className="mt-0.5 text-2xs text-muted">{verdictBy(h)}</div>
                      {h.finding && filedAfterVerdict(h, h.finding.created_at) && (
                        <div className="mt-0.5 text-2xs text-warning" title="The newest finding was filed after this verdict was set">
                          newer finding
                        </div>
                      )}
                    </td>
                    <td className="text-xs">
                      <TextLink onClick={e => e.stopPropagation()} href={`/hypotheses/${h.id}`} className="line-clamp-2" title={h.text}>{h.text}</TextLink>
                    </td>
                    <td className={num}>
                      {h.best_job ? (
                        <TextLink onClick={e => e.stopPropagation()} href={`/jobs/${h.best_job.id}`} title={h.best_job.id}>
                          {h.best_job.metric.name} {h.best_job.metric.value.toFixed(4)}
                        </TextLink>
                      ) : <span className="text-xs text-muted">—</span>}
                      {h.best_job?.summary_due && <div><NoSummary /></div>}
                    </td>
                    <td className="text-xs">
                      {h.best_job?.summary ? (
                        <span className="line-clamp-2" title={h.best_job.summary}>{h.best_job.summary}</span>
                      ) : h.finding ? (
                        <span className="line-clamp-2" title={h.finding.summary}>
                          <TextLink onClick={e => e.stopPropagation()} href={`/jobs/${h.finding.experiment_id}`} className="font-mono">{h.finding.experiment_id}</TextLink>
                          {': '}{h.finding.summary}
                        </span>
                      ) : <span className="text-xs text-muted">no finding filed</span>}
                    </td>
                  </LinkTr>
                ))}
              </tbody>
            </Table>
          </CardContent>
        </Card>
      )}

      {/* Competing agents over time */}
      {metrics.length > 0 && (
        <Card>
          <CardHeader><CardTitle>Agents over time</CardTitle></CardHeader>
          <CardContent>
            <div className={clsx('mb-4 grid gap-6', knownAgentIds.length > 0 ? 'grid-cols-2' : 'grid-cols-1')}>
              <MetricSelector metrics={metrics} selected={activeMetricKeys} onChange={setSelectedMetricKeys} />
              {knownAgentIds.length > 0 && (
                <AgentSelector agentIds={knownAgentIds} selected={activeAgentIds} onChange={setSelectedAgentIds} colorFor={colorFor} />
              )}
            </div>
            {activeMetricKeys.size === 0 ? (
              <EmptyState>Select at least one metric to plot.</EmptyState>
            ) : (
              <div className="grid gap-4">
                {metrics.filter(m => activeMetricKeys.has(m.key)).map(m => (
                  <div key={m.key}>
                    <div className="mb-1 text-xs font-semibold">
                      {m.key} <span className="font-normal text-subtle">({m.direction})</span>
                    </div>
                    <CompetingAgentsChart
                      series={(allMetricSeries ?? []).find(e => e.key === m.key)?.series}
                      metricName={m.key}
                      direction={m.direction}
                      selectedAgents={activeAgentIds}
                      colorFor={colorFor}
                      boundaries={stagesStatus?.advances}
                    />
                  </div>
                ))}
              </div>
            )}
          </CardContent>
        </Card>
      )}

      {/* Stage ladder — boundaries are published, per-agent rank deliberately is not
          (see docs/stages.md). */}
      {stagesStatus && stagesStatus.stages.length > 0 && (
        <Card className={clsx('border-l-[3px]', stagesStatus.cut_agents.length > 0 ? 'border-l-warning hover:border-l-warning' : 'border-l-accent hover:border-l-accent')}>
          <CardContent>
            <div className="flex flex-wrap items-center gap-3">
              <Badge tone="accent">Stage {stagesStatus.current_stage} of {stagesStatus.stages.length}</Badge>
              <span className="text-xs text-subtle">
                {Math.round(stagesStatus.progress * 100)}% through the experiment
                {stagesStatus.current_stage < stagesStatus.stages.length && (
                  <> — next boundary at {Math.round(stagesStatus.next_boundary_progress * 100)}%,
                    cutting {stagesStatus.stages[stagesStatus.current_stage - 1].evict_pct}% of survivors</>
                )}
                {stagesStatus.current_stage >= stagesStatus.stages.length && <> — final stage, no cut remaining</>}
              </span>
            </div>

            {/* The ladder itself: one segment per stage, sized by its share of the experiment. */}
            <div className="mt-3 flex gap-[3px]">
              {stagesStatus.stages.reduce<{ start: number; nodes: JSX.Element[] }>((acc, stage, i) => {
                const stageNo = i + 1
                const end = acc.start + stage.length_pct / 100
                const current = stageNo === stagesStatus.current_stage
                const filled = Math.max(0, Math.min(1, (stagesStatus.progress - acc.start) / (end - acc.start)))
                acc.nodes.push(
                  <div key={stageNo} className="min-w-0" style={{ flex: stage.length_pct }} title={stage.max_job_hours ? `No single job may run longer than ${stage.max_job_hours}h during this stage` : 'No job length limit during this stage'}>
                    <div className={clsx('h-2 overflow-hidden rounded-sm bg-border', current && 'shadow-glow')}>
                      <div className={clsx('h-full', current ? 'bg-accent' : 'bg-accent/55')} style={{ width: `${filled * 100}%` }} />
                    </div>
                    <div className="mt-1 truncate font-mono text-2xs text-subtle">
                      {stage.length_pct}%{stage.evict_pct > 0 && ` · cut ${stage.evict_pct}%`}{stage.max_job_hours ? ` · ≤${stage.max_job_hours}h/job` : ''}
                    </div>
                  </div>
                )
                return { start: end, nodes: acc.nodes }
              }, { start: 0, nodes: [] }).nodes}
            </div>

            <p className="mt-3 text-xs text-muted">
              <span className="font-mono font-semibold text-success">{stagesStatus.active_agents.length}</span> active
              {' · '}<span className={clsx('font-mono font-semibold', stagesStatus.cut_agents.length > 0 && 'text-danger')}>{stagesStatus.cut_agents.length}</span> cut.
              The scoreboard says which.
            </p>
          </CardContent>
        </Card>
      )}

      {/* Stats row */}
      <Card>
        <CardContent>
          <div className="grid grid-cols-[repeat(auto-fit,minmax(220px,1fr))] gap-4">
            <StatTile label="Budget" value={`${pe.budget_accelerator_hours} ${unit}`} />
            <StatTile
              label={held.label}
              value={held.value}
              tone={held.atCap ? 'accent' : undefined}
              sub={pe.max_concurrent_accelerators == null ? 'no cap set' : undefined}
            />
            <StatTile label="Agents" value={pe.signup_count.toString()} sub={`at most ${pe.max_agents} researchers`} />
            <StatTile label="Budget Used" value={`${formatAccH(totalUsed)} ${unit}`} />
            <StatTile label="Jobs" value={(stats?.total ?? 0).toString()} href={`/jobs?platform_experiment_id=${pe.id}`} />
            <StatTile
              label="Running"
              value={(jobsByStatus.RUNNING ?? 0).toString()}
              tone={(jobsByStatus.RUNNING ?? 0) > 0 ? 'success' : undefined}
              href={`/jobs?platform_experiment_id=${pe.id}&status=RUNNING`}
            />
            <StatTile
              label="Evicted"
              value={(jobsByStatus.EVICTED ?? 0).toString()}
              tone={(jobsByStatus.EVICTED ?? 0) > 0 ? 'purple' : undefined}
              sub={[evictionFaultBreakdown, evictionBreakdown].filter(Boolean).join(' — ') || undefined}
              href={`/jobs?platform_experiment_id=${pe.id}&status=EVICTED`}
            />
            <StatTile
              label="Hypotheses"
              value={hypothesisCount.toString()}
              tone="accent"
              sub={hypothesisSub}
              href={graphHref(pe.id)}
            />
          </div>
        </CardContent>
      </Card>

      <ExperimentAgents pe={pe} />

      {/* Scoreboard */}
      <Card>
        <CardHeader>
          <CardTitle note={primaryStandings && `ranked by ${primaryStandings.metric} (${primaryStandings.direction})`}>Scoreboard</CardTitle>
        </CardHeader>
        <CardContent scrollX>
          {scoreboard.length === 0 ? (
            <EmptyState>No agents yet.</EmptyState>
          ) : (
            <Table>
              <thead>
                <tr>
                  <th className="w-9">Rank</th>
                  <th>Agent</th>
                  {metrics.length > 0 ? metrics.map((m, i) => (
                    <th key={m.key} className="text-right">
                      {i === 0 && metrics.length > 1 && (
                        <span title="Primary metric — used for ranking" className="mr-1 font-bold">★</span>
                      )}
                      Best {m.key} <span className="font-normal text-subtle">({m.direction === 'maximize' ? '↑' : '↓'})</span>
                    </th>
                  )) : <th className="text-right">Best Metric</th>}
                  <th title={`The hypothesis each agent's best ${primaryStandings?.metric ?? 'ranked'} job tested, and the settings that differ between those jobs`}>
                    Best job: hypothesis · settings
                  </th>
                  <th className="text-right">Jobs</th>
                  <th className="text-right">Completed</th>
                  <th>Quota Used</th>
                </tr>
              </thead>
              <tbody>
                {scoreboard.map(agentId => {
                  const quota = (quotas ?? []).find(q => q.agent_id === agentId)
                  const usedAccH = quota ? quota.used_guaranteed_acch + quota.used_burst_acch : 0
                  const totalAccH = quota ? quota.share_accelerator_hours : 0
                  const isCut = (stagesStatus?.cut_agents ?? []).some(c => c.agent_id === agentId)
                  const isActive = !isCut && (stagesStatus?.cut_agents.length ?? 0) > 0
                    && (stagesStatus?.active_agents ?? []).includes(agentId)
                  const isExpanded = expandedAgentId === agentId
                  const colCount = 6 + Math.max(metrics.length, 1)
                  const standing = standingOf.get(agentId)
                  const rank = standing?.rank
                  const isWinner = rank === 1
                  const nonRawMetrics = (results?.bests ?? []).filter(b => b.non_raw_basis_agents?.includes(agentId)).map(b => b.metric)
                  const jobs = jobsByAgent[agentId]
                  return (
                    <Fragment key={agentId}>
                      <tr
                        onClick={() => setExpandedAgentId(isExpanded ? null : agentId)}
                        className={clsx('cursor-pointer', isCut ? 'bg-warning/5 opacity-60' : isWinner && 'bg-primary/5')}
                      >
                        <td className="font-mono text-xs font-semibold">{rank != null ? `#${rank}` : '—'}</td>
                        <td className="whitespace-nowrap font-mono text-xs font-semibold">
                          <span className="inline-block w-3 text-muted">{isExpanded ? '▾' : '▸'}</span>
                          <AgentLabel id={agentId} />
                          {isCut && <Badge tone="danger" className="ml-2">Cut</Badge>}
                          {isActive && <Badge tone="success" className="ml-2">Active</Badge>}
                          {nonRawMetrics.length > 0 && (
                            <Badge tone="danger" className="ml-2" title={`Also reported ${nonRawMetrics.join(', ')} on a non-"raw" basis (a rescaled/transformed value). That value is never blended into ${nonRawMetrics.length > 1 ? 'those rankings' : 'that ranking'} — only a raw-basis value (if any) counts here.`}>Non-raw basis</Badge>
                          )}
                        </td>
                        {metrics.length > 0 ? metrics.map(m => {
                          const best = bestOf(m.key, agentId)
                          return (
                            <td key={m.key} className={clsx(num, isWinner && 'font-bold')}>
                              {best?.experiment_id ? (
                                <TextLink
                                  href={`/jobs/${best.experiment_id}`}
                                  title={`The job that set it: ${best.experiment_id}`}
                                  onClick={e => e.stopPropagation()}
                                >
                                  {best.best.toFixed(4)}
                                </TextLink>
                              ) : best ? best.best.toFixed(4) : '—'}
                              {best?.summary_due && <div><NoSummary /></div>}
                            </td>
                          )
                        }) : <td className={num}>—</td>}
                        <td className="max-w-[360px] text-xs">
                          {standing?.hypothesis_id ? (
                            <>
                              <TextLink
                                href={`/hypotheses/${standing.hypothesis_id}`}
                                className="line-clamp-2"
                                title={hypothesisText.get(standing.hypothesis_id)}
                                onClick={e => e.stopPropagation()}
                              >
                                {hypothesisText.get(standing.hypothesis_id) ?? standing.hypothesis_id}
                              </TextLink>
                              <div className="font-mono text-2xs text-muted" title={settings(standing.env, Object.keys(standing.env ?? {}).sort())}>
                                {settings(standing.env, standingKeys)}
                              </div>
                            </>
                          ) : standing?.objective ? (
                            <span className="text-xs text-muted">The baseline: it measures the objective and tests no hypothesis</span>
                          ) : <span className="text-xs text-muted">—</span>}
                        </td>
                        <td className={num}>{jobs?.total ?? 0}</td>
                        <td className={num}>{jobs?.by_status.COMPLETED ?? 0}</td>
                        <td className="w-56">
                          {quota && totalAccH > 0 ? (
                            <>
                              <MetricBar value={usedAccH} max={totalAccH} />
                              <div className="mt-0.5 whitespace-nowrap font-mono text-2xs text-muted" title={`Used of what is guaranteed to the agent and of what it may take beyond that, in ${unit}`}>
                                guaranteed {formatAccH(quota.used_guaranteed_acch)}/{formatAccH(quota.guaranteed_accelerator_hours)}
                                {' · '}burst {formatAccH(quota.used_burst_acch)}/{formatAccH(quota.burst_accelerator_hours)}
                              </div>
                            </>
                          ) : <span className="text-xs text-muted">no quota</span>}
                        </td>
                      </tr>
                      {isExpanded && (
                        <tr>
                          <td colSpan={colCount} className="bg-surface-inset">
                            <AgentJobsPanel
                              projectId={pe.project_id}
                              platformExperimentId={pe.id}
                              agentId={agentId}
                              trackedMetrics={trackedMetricKeys}
                            />
                          </td>
                        </tr>
                      )}
                    </Fragment>
                  )
                })}
              </tbody>
            </Table>
          )}
        </CardContent>
      </Card>

      {/* Live job metrics */}
      {jobRows.length > 0 && (
        <Card>
          <CardHeader><CardTitle note="over training progress, refreshed every 30 s">Job metrics</CardTitle></CardHeader>
          <CardContent>
            <LiveMetricsPicker jobs={jobRows} />
          </CardContent>
        </Card>
      )}

      {/* Jobs */}
      <Card>
        <CardHeader><CardTitle>Jobs</CardTitle></CardHeader>
        <CardContent scrollX>
          {jobRows.length === 0 ? (
            <EmptyState>No jobs submitted yet.</EmptyState>
          ) : (
            <Table>
              <thead>
                <tr>
                  <th>ID</th>
                  <th>Agent</th>
                  <th>Status</th>
                  <th title="Under each hypothesis: the settings that differ between these jobs of it">Hypothesis · varied settings</th>
                  <th className="text-right">Best {primaryStandings?.metric ?? 'Metric'}</th>
                  <th className="text-right">Est. Cost</th>
                  <th>Submitted</th>
                </tr>
              </thead>
              <tbody>
                {jobRows.map(exp => {
                  const cost = exp.estimated_cost_acch
                  return (
                    <LinkTr key={exp.id} href={`/jobs/${exp.id}`}>
                      <td className="font-mono text-2xs">
                        <TextLink onClick={e => e.stopPropagation()} href={`/jobs/${exp.id}`}>{exp.id.slice(0, 8)}…</TextLink>
                      </td>
                      <td className="font-mono text-xs"><AgentLabel id={exp.agent_id} /></td>
                      <td>
                        <Badge status={exp.status} />
                        {exp.summary_due && <NoSummary className="ml-1.5" />}
                        {exp.eviction_reason && (
                          <div className="mt-0.5 text-2xs text-subtle" title="eviction_reason">
                            <JobReason reason={exp.eviction_reason} />
                          </div>
                        )}
                      </td>
                      <td className="max-w-xs truncate text-xs" title={exp.hypothesis}>
                        <JobHypothesis job={exp} />
                        <div className="mt-0.5 whitespace-normal font-mono text-2xs text-muted">
                          {variedEnvSettings(exp, variedByHypothesis)}
                        </div>
                      </td>
                      <td className={num}>{exp.metric?.value.toFixed(4) ?? '—'}</td>
                      <td className={num}>{cost != null ? `${formatAccH(cost)} ${budgetUnit(kindOf(exp.accelerator_type))}` : '—'}</td>
                      <td className="text-xs text-subtle">{new Date(exp.created_at).toLocaleString()}</td>
                    </LinkTr>
                  )
                })}
              </tbody>
            </Table>
          )}
          {(stats?.total ?? 0) > jobRows.length && (
            <TextLink href={`/jobs?platform_experiment_id=${pe.id}`} className="mt-2 inline-block text-sm">
              All {stats?.total} jobs →
            </TextLink>
          )}
        </CardContent>
      </Card>

    </div>
    </ReadOnlyProvider>
  )
}
