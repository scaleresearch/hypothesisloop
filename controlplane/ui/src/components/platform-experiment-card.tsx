'use client'

import { useState } from 'react'
import useSWR from 'swr'
import { clsx } from 'clsx'
import { fetchPlatformExperimentQuotas, fetchExperimentStats, fetchHypothesisStats, HYPOTHESIS_STATUSES } from '@/lib/api'
import type { PlatformExperiment, AgentQuota, MetricDefinition } from '@/types'
import { useProjectRouter } from '@/lib/query-state'
import { platformExperimentHref } from '@/lib/example'
import { useTenant } from '@/lib/tenant'
import { quotaRemainingAccH } from '@/lib/quota'
import { hypothesisActivity } from '@/lib/hypothesis-progress'
import { formatAccH, formatDate, isZeroDate } from '@/lib/format'
import { statusColor } from '@/lib/colors'
import { Card, CardHeader, CardContent } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Table } from '@/components/ui/table'
import { Eyebrow, Pill } from '@/components/ui/text'
import { CollapsibleDescription } from '@/components/ui/collapsible-description'

function StatusBadge({ status }: { status: string }) {
  return <Badge status={status}>{status}</Badge>
}

// The primary metric ranks the leaderboard, so its pill carries the accent ring.
function MetricChip({ m, primary }: { m: MetricDefinition; primary?: boolean }) {
  return (
    <Pill tone={m.direction === 'maximize' ? 'success' : 'warning'} active={primary} title={primary ? 'Primary metric — used for ranking' : undefined}>
      {primary && <span className="opacity-80">★</span>}
      {m.key} {m.direction === 'maximize' ? '↑' : '↓'}
    </Pill>
  )
}

/**
 * One platform experiment in a list, opening its page. Without `onEdit` it offers no edit, which
 * is how a list of examples shows it.
 */
export function PlatformExperimentCard({
  pe,
  onEdit,
}: {
  pe: PlatformExperiment
  onEdit?: (pe: PlatformExperiment) => void
}) {
  const router = useProjectRouter()
  const { me } = useTenant()
  const [expanded, setExpanded] = useState(false)
  const href = platformExperimentHref(me, pe)

  // Fetched unconditionally (not gated on `expanded`) — the Budget Used stat tile above the
  // expand toggle needs this data too, otherwise it shows 0 until the card is expanded once.
  const { data: quotas } = useSWR<AgentQuota[]>(
    ['pe-quotas', pe.id],
    () => fetchPlatformExperimentQuotas(pe.id),
    { refreshInterval: 10_000 },
  )

  const { data: verdicts } = useSWR(
    ['pe-hypothesis-stats', pe.id],
    () => fetchHypothesisStats(pe.id),
    { refreshInterval: 15_000 },
  )
  const { data: hypothesisStats } = useSWR(
    ['pe-stats-by-hypothesis', pe.id],
    () => fetchExperimentStats({ project_id: pe.project_id, platform_experiment_id: pe.id, group_by: 'hypothesis' }),
    { refreshInterval: 15_000 },
  )
  const hypothesisCount = verdicts?.total ?? 0
  const activity = hypothesisActivity(hypothesisCount, hypothesisStats?.groups ?? {})

  const totalUsed = (quotas ?? []).reduce((s, q) => s + q.used_guaranteed_acch + q.used_burst_acch, 0)
  const daysLeft = !isZeroDate(pe.ends_at)
    ? Math.ceil((new Date(pe.ends_at!).getTime() - Date.now()) / 86_400_000)
    : null

  return (
    <Card className="cursor-pointer">
      <CardHeader
        className="justify-between"
        onClick={() => router.push(href)}
        tabIndex={0}
        role="link"
        onKeyDown={e => { if (e.key === 'Enter') router.push(href) }}
      >
        <div className="flex items-center gap-2.5">
          <StatusBadge status={pe.status} />
          <span className="text-base font-bold">{pe.name}</span>
        </div>
        <div className="flex items-center gap-3 font-normal">
          {!isZeroDate(pe.starts_at) && (
            <span className="text-xs text-subtle">{formatDate(pe.starts_at)} – {formatDate(pe.ends_at)}</span>
          )}
          {daysLeft != null && daysLeft > 0 && (
            <span className={clsx('text-2xs', daysLeft < 2 ? 'text-danger' : 'text-muted')}>{daysLeft}d remaining</span>
          )}
          {onEdit && <Button size="sm" onClick={e => { e.stopPropagation(); onEdit(pe) }}>Edit</Button>}
          <span className="text-xs text-muted" onClick={e => { e.stopPropagation(); setExpanded(v => !v) }}>{expanded ? '▲' : '▼'}</span>
        </div>
      </CardHeader>

      <CardContent onClick={() => router.push(href)}>
        {pe.description && <CollapsibleDescription text={pe.description} className="mb-3" />}

        {pe.metrics && pe.metrics.length > 0 && (
          <div className="mb-3.5 flex flex-wrap gap-1.5">
            {pe.metrics.map((m, i) => <MetricChip key={m.key} m={m} primary={i === 0} />)}
          </div>
        )}

        {pe.stages && pe.stages.length > 0 && (
          <div className="mb-3.5 flex flex-wrap items-center gap-1.5">
            <Eyebrow className="mr-0.5">Ladder</Eyebrow>
            {pe.stages.map((s, i) => {
              const active = i + 1 === pe.current_stage
              return (
                <Pill
                  key={i}
                  tone={active ? 'accent' : undefined}
                  active={active}
                  title={`Stage ${i + 1}: spans ${s.length_pct}% of the run${s.evict_pct > 0 ? `, then cuts ${s.evict_pct}% of survivors` : ', no cut'}${s.max_job_hours ? `; no job may run longer than ${s.max_job_hours}h` : '; no job length limit'}`}
                >
                  {s.length_pct}% {s.evict_pct > 0 ? `→ cut ${s.evict_pct}%` : '→ no cut'}{s.max_job_hours ? ` · ≤${s.max_job_hours}h/job` : ''}
                </Pill>
              )
            })}
          </div>
        )}

        <div className="mb-3.5 grid grid-cols-4 gap-4">
          <div>
            <Eyebrow className="block">Budget</Eyebrow>
            <div className="font-mono text-xl font-bold">{pe.budget_accelerator_hours} <span className="text-xs text-muted">AccH</span></div>
          </div>
          <div>
            <Eyebrow className="block">Agents</Eyebrow>
            <div className="font-mono text-xl font-bold">{pe.signup_count} <span className="text-xs text-muted">/ {pe.max_agents}</span></div>
          </div>
          <div>
            <Eyebrow className="block">Budget Used</Eyebrow>
            <div className={clsx('font-mono text-xl font-bold', totalUsed > 0 && 'text-accent')}>
              {formatAccH(totalUsed)} <span className="text-xs text-muted">AccH</span>
            </div>
          </div>
          <div>
            <Eyebrow className="block">Hypotheses</Eyebrow>
            <div className="flex flex-wrap items-baseline gap-2.5">
              {HYPOTHESIS_STATUSES.map(v => (
                <span key={v} className="font-mono text-sm" style={{ color: statusColor(v) }}>
                  {verdicts?.by_status[v] ?? 0} <span className="text-2xs text-muted">{v}</span>
                </span>
              ))}
            </div>
            {activity && <div className="text-2xs text-muted">{activity}</div>}
          </div>
        </div>

        {expanded && (
          <div onClick={e => e.stopPropagation()}>
            <div className="border-t pt-3.5">
              <Eyebrow className="mb-2.5 block">
                Agent Quotas — Guaranteed &amp; Burst (AccH)
              </Eyebrow>
              {!quotas ? (
                <p className="text-xs text-subtle">Loading quotas…</p>
              ) : quotas.length === 0 ? (
                <p className="text-xs text-subtle">No quotas allocated yet.</p>
              ) : (
                <Table>
                  <thead>
                    <tr>
                      <th>Agent</th>
                      <th>Guaranteed</th>
                      <th>Burst</th>
                      <th>Remaining</th>
                    </tr>
                  </thead>
                  <tbody>
                    {quotas.map(q => {
                      const remaining = quotaRemainingAccH(q)
                      return (
                        <tr key={q.id}>
                          <td className="font-mono text-xs font-semibold">{q.agent_id}</td>
                          <td className="font-mono text-2xs">{formatAccH(q.used_guaranteed_acch)} / {formatAccH(q.guaranteed_accelerator_hours)} AccH</td>
                          <td className="font-mono text-2xs">{formatAccH(q.used_burst_acch)} / {formatAccH(q.burst_accelerator_hours)} AccH</td>
                          <td className={clsx('font-mono text-2xs', remaining > 0 ? 'text-success' : 'text-danger')}>{formatAccH(remaining)} AccH</td>
                        </tr>
                      )
                    })}
                  </tbody>
                </Table>
              )}
            </div>
          </div>
        )}
      </CardContent>
    </Card>
  )
}
