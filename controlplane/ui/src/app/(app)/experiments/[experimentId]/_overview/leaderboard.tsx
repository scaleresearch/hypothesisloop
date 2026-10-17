'use client'

import { Fragment, useState } from 'react'
import { clsx } from 'clsx'
import { AgentLabel } from '@/components/agent-label'
import { Badge } from '@/components/ui/badge'
import { EmptyState } from '@/components/ui/empty-state'
import { TextLink } from '@/components/ui/link'
import { MetricBar } from '@/components/ui/metric-bar'
import { Section } from '@/components/ui/section'
import { Table, num } from '@/components/ui/table'
import { Text } from '@/components/ui/text'
import { formatAccH } from '@/lib/format'
import { settings, variedKeys } from '@/lib/hypothesis-progress'
import { hypothesisPath, jobPath } from '@/lib/routes'
import { AgentJobsPanel } from './job-metrics'
import { bestOf } from './standings'
import type { AgentQuotaView, AgentStanding, ExperimentStats, PlatformExperiment, PlatformExperimentResults, StagesStatus } from '@/types/api'

/**
 * The agents ranked as the server ranks them on the primary ranking metric; an agent without a
 * standing (not a competitor, ineligible, or nothing reported) follows, by id. A row opens on
 * the agent's jobs.
 */
export function Leaderboard({ pe, agentIds, results, quotas, stages, jobsByAgent, hypothesisText, unit }: {
  pe: PlatformExperiment
  agentIds: string[]
  results: PlatformExperimentResults | undefined
  quotas: AgentQuotaView[] | undefined
  stages: StagesStatus | undefined
  jobsByAgent: Record<string, ExperimentStats>
  hypothesisText: Map<string, string>
  unit: string
}) {
  const [expandedAgentId, setExpandedAgentId] = useState<string | null>(null)
  const metrics = pe.metrics ?? []
  const primary = results?.metrics[0]
  const standingOf = new Map((primary?.standings ?? []).map(st => [st.agent_id, st]))
  const unranked = Number.MAX_SAFE_INTEGER
  const scoreboard = [...agentIds].sort((a, b) => (standingOf.get(a)?.rank ?? unranked) - (standingOf.get(b)?.rank ?? unranked))
  // What tells the agents' best jobs apart; a lone agent's best job shows every setting.
  const standingEnvs = (primary?.standings ?? []).map(st => st.env)
  const standingKeys = standingEnvs.length > 1 ? variedKeys(standingEnvs) : Object.keys(standingEnvs[0] ?? {}).sort()

  return (
    <Section title="Leaderboard" note={primary && `ranked by ${primary.metric} (${primary.direction})`} scrollX>
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
              <th title={`The hypothesis each agent's best ${primary?.metric ?? 'ranked'} job tested, and the settings that differ between those jobs`}>
                Best job: hypothesis · settings
              </th>
              <th className="text-right">Jobs</th>
              <th className="text-right">Completed</th>
              <th>Quota Used</th>
            </tr>
          </thead>
          <tbody>
            {scoreboard.map(agentId => {
              const isExpanded = expandedAgentId === agentId
              return (
                <Fragment key={agentId}>
                  <LeaderboardRow
                    pe={pe}
                    agentId={agentId}
                    standing={standingOf.get(agentId)}
                    standingKeys={standingKeys}
                    results={results}
                    quota={(quotas ?? []).find(q => q.agent_id === agentId)}
                    stages={stages}
                    jobs={jobsByAgent[agentId]}
                    hypothesisText={hypothesisText}
                    unit={unit}
                    expanded={isExpanded}
                    onToggle={() => setExpandedAgentId(isExpanded ? null : agentId)}
                  />
                  {isExpanded && (
                    <tr>
                      <td colSpan={6 + Math.max(metrics.length, 1)} className="bg-surface-inset">
                        <AgentJobsPanel
                          projectId={pe.project_id}
                          platformExperimentId={pe.id}
                          agentId={agentId}
                          trackedMetrics={metrics.map(m => m.key)}
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
    </Section>
  )
}

function LeaderboardRow({ pe, agentId, standing, standingKeys, results, quota, stages, jobs, hypothesisText, unit, expanded, onToggle }: {
  pe: PlatformExperiment
  agentId: string
  standing: AgentStanding | undefined
  standingKeys: string[]
  results: PlatformExperimentResults | undefined
  quota: AgentQuotaView | undefined
  stages: StagesStatus | undefined
  jobs: ExperimentStats | undefined
  hypothesisText: Map<string, string>
  unit: string
  expanded: boolean
  onToggle: () => void
}) {
  const metrics = pe.metrics ?? []
  const usedAccH = quota ? quota.used_guaranteed_acch + quota.used_burst_acch : 0
  const totalAccH = quota ? quota.share_accelerator_hours : 0
  const isCut = (stages?.cut_agents ?? []).some(c => c.agent_id === agentId)
  const isActive = !isCut && (stages?.cut_agents.length ?? 0) > 0
    && (stages?.active_agents ?? []).includes(agentId)
  const rank = standing?.rank
  const isWinner = rank === 1
  const nonRawMetrics = (results?.bests ?? []).filter(b => b.non_raw_basis_agents?.includes(agentId)).map(b => b.metric)

  return (
    <tr
      onClick={onToggle}
      className={clsx('cursor-pointer', isCut ? 'bg-danger-faint' : isWinner && 'bg-success-faint')}
    >
      <td className="font-mono text-xs font-medium">{rank != null ? `#${rank}` : '—'}</td>
      <td className="whitespace-nowrap font-mono text-xs font-medium">
        <span className="inline-block w-3 text-muted">{expanded ? '▾' : '▸'}</span>
        <AgentLabel id={agentId} />
        {isCut && <Badge tone="danger" className="ml-2">Cut</Badge>}
        {isActive && <Badge tone="success" className="ml-2">Active</Badge>}
        {nonRawMetrics.length > 0 && (
          <Badge tone="danger" className="ml-2" title={`Also reported ${nonRawMetrics.join(', ')} on a non-"raw" basis (a rescaled/transformed value). That value is never blended into ${nonRawMetrics.length > 1 ? 'those rankings' : 'that ranking'}. Only a raw-basis value, if any, counts here.`}>Non-raw basis</Badge>
        )}
      </td>
      {metrics.length > 0 ? metrics.map(m => {
        const best = bestOf(results, m.key, agentId)
        return (
          <td key={m.key} className={clsx(num, isWinner && 'font-medium')}>
            {best?.experiment_id ? (
              <TextLink
                href={jobPath(pe.id, best.experiment_id)}
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
              href={hypothesisPath(pe.id, standing.hypothesis_id)}
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
            <Text as="div" variant="meta" mono className="whitespace-nowrap" title={`Reserved of what is guaranteed to the agent and of what it may take beyond that, in ${unit}: a job not started at its estimate, a running one at what it has spent so far`}>
              guaranteed {formatAccH(quota.used_guaranteed_acch)}/{formatAccH(quota.guaranteed_accelerator_hours)}
              {' · '}burst {formatAccH(quota.used_burst_acch)}/{formatAccH(quota.burst_accelerator_hours)}
            </Text>
          </>
        ) : <span className="text-xs text-muted">no quota</span>}
      </td>
    </tr>
  )
}
