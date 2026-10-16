'use client'

import { Fragment, useState } from 'react'
import { useRouter } from 'next/navigation'
import { isReadOnly, ReadOnlyProvider } from '@/lib/read-only'
import { experimentPath, hypothesisPath, jobPath, projectPath, withQuery } from '@/lib/routes'
import { useTenant } from '@/lib/tenant'
import { SharedBadge } from '@/components/shared-badge'
import { TextLink } from '@/components/ui/link'
import useSWR from 'swr'
import { clsx } from 'clsx'
import { fetchFindings, fetchHypothesisStats } from '@/lib/api/hypotheses'
import { fetchExperimentsPage, fetchExperimentStats } from '@/lib/api/jobs'
import { fetchPlatformExperiment, fetchPlatformExperimentResults, fetchPlatformExperimentTimeseries, fetchStages } from '@/lib/api/platform-experiments'
import { fetchPlatformExperimentQuotas } from '@/lib/api/quotas'
import { HYPOTHESIS_STATUSES } from '@/types/hypotheses'
import type { Draft } from '@/types/planning'
import { PlatformExperimentStatus } from '@/types/platform-experiments'
import type { AgentMetricSeries, PlatformExperiment, StagesStatus } from '@/types/platform-experiments'
import type { AgentQuota } from '@/types/quotas'
import { Baseline } from '@/components/baseline'
import { JobHypothesis } from '@/components/job-hypothesis'
import { DraftView } from '@/components/draft'
import { CloseExperiment } from '@/components/close-experiment'
import { RemoveExperiment } from '@/components/remove-experiment'
import { PhaseStrip } from '@/components/phase-strip'
import { AgentJobsPanel, AgentSelector, CompetingAgentsChart, LiveMetricsPicker, MetricSelector, useAgentColorMap } from '@/components/experiment-charts'
import { ExperimentAgents } from '@/components/experiment-agents'
import { ResearcherPool } from '@/components/researcher-pool'
import { HostedMachines } from '@/components/hosted-machines'
import { AgentLabel, AgentNames } from '@/components/agent-label'
import { verdictBy } from '@/components/actor'
import { Captioned } from '@/components/ui/captioned'
import { Section } from '@/components/ui/section'
import { Inline, Stack } from '@/components/ui/stack'
import { Badge } from '@/components/ui/badge'
import { MetricBar } from '@/components/ui/metric-bar'
import { CollapsibleDescription } from '@/components/ui/collapsible-description'
import { Disclosure } from '@/components/ui/disclosure'
import { Figures } from '@/components/ui/figures'
import type { Figure } from '@/components/ui/figures'
import { Page } from '@/components/ui/page'
import { Button } from '@/components/ui/button'
import { PlatformExperimentDialog } from '@/components/platform-experiment-form'
import { EmptyState } from '@/components/ui/empty-state'
import { ExperimentCode } from '@/components/experiment-code'
import { Loading, ErrorMessage } from '@/components/ui/status-message'
import { LinkTr, RowAnchor, Table, num } from '@/components/ui/table'
import { Pill, Text } from '@/components/ui/text'
import { semantic } from '@/lib/colors'
import { formatAccH, headline, relTime, runClock, shortId } from '@/lib/format'
import { evictionLabel, faultBreakdown } from '@/lib/eviction'
import { ReferenceValues } from '@/components/reference-values'
import { filedAfterVerdict, hypothesisActivity, settings, variedEnvByHypothesis, variedEnvSettings, variedKeys } from '@/lib/hypothesis-progress'
import { graphHref } from '@/lib/hypothesis-graph'
import { budgetKind, budgetUnit, inFlight, kindOf } from '@/lib/units'
import { phaseOf } from '@/lib/phase'
import { pollEvery, useRoster } from '@/lib/roster'

// How many of the newest jobs the page reads, for the jobs table and the metrics picker.
const RECENT_JOBS = 20

// The reasons a draft closes for, before anything was accepted.
const PLANNED_ONLY: NonNullable<PlatformExperiment['close_reason']>[] = ['planning_limit_reached', 'proposal_expired', 'planner_stopped', 'repository_not_created']

export default function PlatformExperimentDetailPage({ params }: { params: { experimentId: string } }) {
  return <AgentNames platformExperimentID={params.experimentId}><PlatformExperimentDetail id={params.experimentId} /></AgentNames>
}

function PlatformExperimentDetail({ id }: { id: string }) {
  const router = useRouter()
  const [selectedAgentIds, setSelectedAgentIds] = useState<Set<string> | null>(null)
  const [selectedMetricKeys, setSelectedMetricKeys] = useState<Set<string> | null>(null)
  const [expandedAgentId, setExpandedAgentId] = useState<string | null>(null)

  const [editing, setEditing] = useState(false)
  const { data: pe, error: peError, isLoading: peLoading, mutate: mutatePE } = useSWR<PlatformExperiment>(
    ['pe', id],
    () => fetchPlatformExperiment(id),
    { refreshInterval: pollEvery },
  )
  const { me, project } = useTenant()
  const { data: roster } = useRoster(pe)

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
  // Nobody is ranked and nothing is charted before the run starts: until then the phase strip says what happens.
  const phase = phaseOf(pe)
  const started = phase === 'running' || phase === 'closed'
  // A draft that closed never had researchers: its max_agents is still the most its planner could propose.
  const plannedOnly = pe.close_reason != null && PLANNED_ONLY.includes(pe.close_reason)
  const researchers = roster?.filter(agent => agent.role === 'competitor').length
  const evicted = jobsByStatus.EVICTED ?? 0
  const runningJobs = jobsByStatus.RUNNING ?? 0
  const leader = primaryStandings?.standings.find(st => st.rank === 1)

  const figures: Figure[] = []
  if (pe.started_at) {
    figures.push({ label: 'Budget used', value: `${formatAccH(totalUsed)} of ${pe.budget_accelerator_hours} ${unit}` })
  } else if (!plannedOnly) {
    figures.push({ label: 'Budget', value: `${pe.budget_accelerator_hours} ${unit}` })
  }
  if (phase === 'running') {
    figures.push({
      label: held.label,
      value: held.value,
      tone: held.atCap ? 'warning' : undefined,
      title: pe.max_concurrent_accelerators == null ? 'No cap set' : undefined,
    })
  }
  if (!plannedOnly) figures.push({ label: 'Jobs', value: stats?.total ?? 0, href: experimentPath(pe.id, 'jobs') })
  if (runningJobs > 0) figures.push({ label: 'Running', value: runningJobs, tone: 'success', href: withQuery(experimentPath(pe.id, 'jobs'), { status: 'RUNNING' }) })
  if (evicted > 0) {
    figures.push({
      label: 'Evicted',
      value: evicted,
      tone: 'warning',
      title: [evictionFaultBreakdown, evictionBreakdown].filter(Boolean).join('. ') || undefined,
      href: withQuery(experimentPath(pe.id, 'jobs'), { status: 'EVICTED' }),
    })
  }
  if (!plannedOnly) figures.push({ label: 'Hypotheses', value: hypothesisCount, title: hypothesisSub, href: graphHref(pe.id) })
  if (!plannedOnly && researchers !== undefined) {
    figures.push({
      label: 'Researchers',
      value: `${researchers} of ${pe.max_agents}`,
      title: 'Researchers who joined, of the most that may join over the run. Baseline agents and the coordinator are not counted.',
    })
  }

  const chartSection = started && metrics.length > 0 && (
    <Section title="Researchers over time">
      <div className={clsx('mb-4 grid gap-6', knownAgentIds.length > 0 ? 'grid-cols-2' : 'grid-cols-1')}>
        <MetricSelector metrics={metrics} selected={activeMetricKeys} onChange={setSelectedMetricKeys} />
        {knownAgentIds.length > 0 && (
          <AgentSelector agentIds={knownAgentIds} selected={activeAgentIds} onChange={setSelectedAgentIds} colorFor={colorFor} />
        )}
      </div>
      {activeMetricKeys.size === 0 ? (
        <EmptyState>Select at least one metric to plot.</EmptyState>
      ) : (
        <Stack gap="lg">
          {metrics.filter(m => activeMetricKeys.has(m.key)).map(m => (
            <div key={m.key}>
              <Text as="div" variant="note" tone="default" className="mb-1 font-medium">
                {m.key} <span className="font-normal text-subtle">({m.direction})</span>
              </Text>
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
        </Stack>
      )}
    </Section>
  )

  const findingsSection = found.length > 0 && (
    <Section title="Findings" note={`${found.length} hypotheses with a verdict or a result`} scrollX>
      <Table className="min-w-3xl table-fixed">
        <thead>
          <tr>
            <th className="w-28">Verdict</th>
            <th>Hypothesis</th>
            <th className="w-44 text-right">Best job</th>
            <th title="The best job's own summary. Without one, the newest finding of the hypothesis, named with the job it is about.">Finding</th>
          </tr>
        </thead>
        <tbody>
          {found.map(h => (
            <LinkTr key={h.id} href={hypothesisPath(id, h.id)} className="*:align-top">
              <td>
                <Badge status={h.status}>{h.status}</Badge>
                <Text as="div" variant="meta">{verdictBy(h)}</Text>
                {h.finding && filedAfterVerdict(h, h.finding.created_at) && (
                  <Text as="div" variant="meta" tone="warning" title="The newest finding was filed after this verdict was set">
                    newer finding
                  </Text>
                )}
              </td>
              <td className="text-xs">
                <TextLink onClick={e => e.stopPropagation()} href={hypothesisPath(id, h.id)} className="line-clamp-2" title={h.text}>{h.text}</TextLink>
              </td>
              <td className={num}>
                {h.best_job ? (
                  <TextLink onClick={e => e.stopPropagation()} href={jobPath(id, h.best_job.id)} title={h.best_job.id}>
                    {h.best_job.metric.name} {h.best_job.metric.value.toFixed(4)}
                  </TextLink>
                ) : <span className="text-xs text-muted">—</span>}
              </td>
              <td className="text-xs">
                {h.best_job?.summary ? (
                  <span className="line-clamp-2" title={h.best_job.summary}>{h.best_job.summary}</span>
                ) : h.finding ? (
                  <span className="line-clamp-2" title={h.finding.summary}>
                    <TextLink onClick={e => e.stopPropagation()} href={jobPath(id, h.finding.experiment_id)} title={h.finding.experiment_id}>{shortId(h.finding.experiment_id)}</TextLink>
                    {': '}{h.finding.summary}
                  </span>
                ) : <span className="text-xs text-muted">no finding filed</span>}
              </td>
            </LinkTr>
          ))}
        </tbody>
      </Table>
    </Section>
  )

  return (
    <ReadOnlyProvider value={readOnly}>
    <Page
      back={{ label: 'Experiments', onClick: () => router.push(projectPath(project.id, 'experiments')) }}
      title={pe.name}
      actions={!readOnly && (
        <Inline gap="sm">
          {pe.status !== PlatformExperimentStatus.CLOSED && (
            <>
              <Button size="sm" onClick={() => setEditing(true)}>Edit</Button>
              <CloseExperiment pe={pe} onClosed={() => void mutatePE()} />
            </>
          )}
          <RemoveExperiment pe={pe} onRemoved={() => router.push(projectPath(project.id, 'experiments'))} />
        </Inline>
      )}
      description={
        <Inline gap="md">
          <Badge status={pe.status}>{pe.status}</Badge>
          <SharedBadge pe={pe} />
          <ExperimentCode pe={pe} />
          {clock && <Text as="span" variant="hint">{clock}</Text>}
        </Inline>
      }
    >
      {editing && <PlatformExperimentDialog initial={pe} onClose={() => setEditing(false)} onSaved={() => void mutatePE()} />}
      <PhaseStrip pe={pe} jobs={jobRows} stages={stagesStatus} readOnly={readOnly}>
        <HostedMachines pe={pe} jobs={stats?.total ?? 0} />
      </PhaseStrip>

      <Baseline pe={pe} />
      <Figures label="This experiment in figures" figures={figures} />

      {phase === 'closed' && (pe.summary || leader) && (
        <Section title="Outcome">
          <Stack>
            {leader && (
              <Text>
                Best {primaryStandings?.metric}: <span className="font-medium tabular-nums">{bestOf(primaryStandings!.metric, leader.agent_id)?.best.toFixed(4) ?? '—'}</span>
                {' '}by <AgentLabel id={leader.agent_id} />
                {leader.hypothesis_id && <>, testing <TextLink inline href={hypothesisPath(id, leader.hypothesis_id)}>{headline(hypothesisText.get(leader.hypothesis_id) ?? 'its hypothesis')}</TextLink></>}.
              </Text>
            )}
            {pe.summary && (
              <Captioned caption="The coordinator's summary">
                <Text pre>{pe.summary}</Text>
              </Captioned>
            )}
          </Stack>
        </Section>
      )}

      {phase === 'closed' && findingsSection}

      {started && <Section title="Leaderboard" note={primaryStandings && `ranked by ${primaryStandings.metric} (${primaryStandings.direction})`} scrollX>
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
                        <span title="The primary metric, which ranks the researchers" className="mr-1 font-medium">★</span>
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
                        className={clsx('cursor-pointer', isCut ? 'bg-danger-faint' : isWinner && 'bg-success-faint')}
                      >
                        <td className="font-mono text-xs font-medium">{rank != null ? `#${rank}` : '—'}</td>
                        <td className="whitespace-nowrap font-mono text-xs font-medium">
                          <span className="inline-block w-3 text-muted">{isExpanded ? '▾' : '▸'}</span>
                          <AgentLabel id={agentId} />
                          {isCut && <Badge tone="danger" className="ml-2">Cut</Badge>}
                          {isActive && <Badge tone="success" className="ml-2">Active</Badge>}
                          {nonRawMetrics.length > 0 && (
                            <Badge tone="danger" className="ml-2" title={`Also reported ${nonRawMetrics.join(', ')} on a non-"raw" basis (a rescaled/transformed value). That value is never blended into ${nonRawMetrics.length > 1 ? 'those rankings' : 'that ranking'}. Only a raw-basis value, if any, counts here.`}>Non-raw basis</Badge>
                          )}
                        </td>
                        {metrics.length > 0 ? metrics.map(m => {
                          const best = bestOf(m.key, agentId)
                          return (
                            <td key={m.key} className={clsx(num, isWinner && 'font-medium')}>
                              {best?.experiment_id ? (
                                <TextLink
                                  href={jobPath(id, best.experiment_id)}
                                  title={`The job that set it: ${best.experiment_id}`}
                                  onClick={e => e.stopPropagation()}
                                >
                                  {best.best.toFixed(4)}
                                </TextLink>
                              ) : best ? best.best.toFixed(4) : '—'}
                            </td>
                          )
                        }) : <td className={num}>—</td>}
                        <td className="max-w-sm text-xs">
                          {standing?.hypothesis_id ? (
                            <>
                              <TextLink
                                href={hypothesisPath(id, standing.hypothesis_id)}
                                className="line-clamp-2"
                                title={hypothesisText.get(standing.hypothesis_id)}
                                onClick={e => e.stopPropagation()}
                              >
                                {hypothesisText.get(standing.hypothesis_id) ?? standing.hypothesis_id}
                              </TextLink>
                              <Text as="div" variant="meta" mono title={settings(standing.env, Object.keys(standing.env ?? {}).sort())}>
                                {settings(standing.env, standingKeys)}
                              </Text>
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
                              <Text as="div" variant="meta" mono className="whitespace-nowrap" title={`Used of what is guaranteed to the agent and of what it may take beyond that, in ${unit}`}>
                                guaranteed {formatAccH(quota.used_guaranteed_acch)}/{formatAccH(quota.guaranteed_accelerator_hours)}
                                {' · '}burst {formatAccH(quota.used_burst_acch)}/{formatAccH(quota.burst_accelerator_hours)}
                              </Text>
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
      </Section>}

      {results?.references && <ReferenceValues experimentId={id} references={results.references} />}

      {phase === 'running' && findingsSection}
      {chartSection}

      {!plannedOnly && <ResearcherPool pe={pe} />}
      <ExperimentAgents pe={pe} />

      {(pe.description || metrics.length > 0) && (
        <Section title="About this experiment">
          <Stack>
            {pe.description && <CollapsibleDescription text={pe.description} />}
            {metrics.length > 0 && (
              <Captioned caption="Optimization Metrics">
                <Inline>
                  {metrics.map((m, i) => (
                    <Pill key={m.key} active={i === 0}>
                      {i === 0 && <span className="font-medium">PRIMARY</span>}
                      {m.key} {m.direction === 'maximize' ? '↑ maximize' : '↓ minimize'}
                    </Pill>
                  ))}
                </Inline>
              </Captioned>
            )}
          </Stack>
        </Section>
      )}

      {jobRows.length > 0 && (
        <Section title="Job metrics" note="over training progress, refreshed every 30 s">
          <Disclosure label="Show the metrics of one job">
            <LiveMetricsPicker jobs={jobRows} />
          </Disclosure>
        </Section>
      )}

      <Section title="Jobs" scrollX>
          {jobRows.length === 0 ? (
            <EmptyState>No jobs submitted yet.</EmptyState>
          ) : (
            <Table className="min-w-[44rem] table-fixed">
              <thead>
                <tr>
                  <th className="w-24">Job</th>
                  <th className="hidden w-32 xl:table-cell">Agent</th>
                  <th className="w-36">Status</th>
                  <th title="What the job tests. Hover a row for the settings that differ between these jobs of it">Hypothesis</th>
                  <th className="w-24 text-right" title={`The last value the job reported on ${primaryStandings?.metric ?? 'the ranking metric'}, which it is ranked by`}>Last metric</th>
                  <th className="w-32 text-right">Est. cost</th>
                  <th className="hidden w-28 xl:table-cell">Submitted</th>
                </tr>
              </thead>
              <tbody>
                {jobRows.map(exp => {
                  const cost = exp.estimated_cost_acch != null ? `${formatAccH(exp.estimated_cost_acch)} ${budgetUnit(kindOf(exp.accelerator_type))}` : '—'
                  const varied = variedEnvSettings(exp, variedByHypothesis)
                  return (
                    <LinkTr key={exp.id} href={jobPath(id, exp.id)}>
                      <td className="whitespace-nowrap font-mono text-2xs" title={exp.id}><RowAnchor>{shortId(exp.id)}</RowAnchor></td>
                      <td className="hidden truncate text-xs xl:table-cell"><AgentLabel id={exp.agent_id} /></td>
                      <td className="truncate" title={exp.eviction_reason}><Badge status={exp.status} /></td>
                      <td className="truncate text-xs" title={[exp.hypothesis, varied].filter(Boolean).join('\n')}>
                        <JobHypothesis job={exp} />
                        {varied && <span className="ml-2 font-mono text-2xs text-subtle">{varied}</span>}
                      </td>
                      <td className={clsx(num, 'truncate')} title={exp.metric?.name}>{exp.metric?.value.toFixed(4) ?? '—'}</td>
                      <td className={clsx(num, 'truncate')} title={cost}>{cost}</td>
                      <td className="hidden truncate text-xs text-subtle xl:table-cell" title={new Date(exp.created_at).toLocaleString()}>{relTime(exp.created_at)}</td>
                    </LinkTr>
                  )
                })}
              </tbody>
            </Table>
          )}
          {(stats?.total ?? 0) > jobRows.length && (
            <TextLink href={experimentPath(pe.id, 'jobs')} className="mt-2 inline-block text-sm">
              All {stats?.total} jobs →
            </TextLink>
          )}
      </Section>
    </Page>
    </ReadOnlyProvider>
  )
}
