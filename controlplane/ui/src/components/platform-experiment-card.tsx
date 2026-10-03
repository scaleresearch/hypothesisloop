'use client'

import useSWR from 'swr'
import { clsx } from 'clsx'
import { fetchPlatformExperimentQuotas, fetchExperimentStats, fetchHypothesisStats, HYPOTHESIS_STATUSES } from '@/lib/api'
import type { PlatformExperiment, AgentQuota, MetricDefinition } from '@/types'
import { useProjectRouter } from '@/lib/query-state'
import { isReadOnly } from '@/lib/read-only'
import { useTenant } from '@/lib/tenant'
import { hypothesisActivity } from '@/lib/hypothesis-progress'
import { formatAccH, isZeroDate, runClock } from '@/lib/format'
import { budgetKind, budgetUnit } from '@/lib/units'
import type { Draft } from '@/lib/planning'
import { statusColor } from '@/lib/colors'
import { Card, CardHeader, CardContent, CardTitle } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import { SharedBadge } from '@/components/shared-badge'
import { Button } from '@/components/ui/button'
import { Eyebrow, Pill } from '@/components/ui/text'
import { CollapsibleDescription } from '@/components/ui/collapsible-description'

function StatusBadge({ status }: { status: string }) {
  return <Badge status={status}>{status}</Badge>
}

// The primary metric ranks the leaderboard, so its pill carries the accent ring.
function MetricChip({ m, primary }: { m: MetricDefinition; primary?: boolean }) {
  return (
    <Pill active={primary} title={primary ? 'Primary metric — used for ranking' : undefined}>
      {primary && <span className="opacity-80">★</span>}
      {m.key} {m.direction === 'maximize' ? '↑' : '↓'}
    </Pill>
  )
}

/** One platform experiment in a list, opening its page. One the caller only reads offers no edit. */
export function PlatformExperimentCard({
  pe,
  onEdit,
}: {
  pe: PlatformExperiment
  onEdit: (pe: PlatformExperiment) => void
}) {
  const router = useProjectRouter()
  const { me } = useTenant()
  const readOnly = isReadOnly(me, pe)
  const href = `/platform-experiments/${pe.id}`

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
  const activity = hypothesisActivity(hypothesisCount, hypothesisStats?.groups ?? {}, pe.id)

  const totalUsed = (quotas ?? []).reduce((s, q) => s + q.used_guaranteed_acch + q.used_burst_acch, 0)
  const unit = budgetUnit(budgetKind((pe as Draft).proposal?.accelerator_type, []))
  const clock = runClock(pe)
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
          <CardTitle>{pe.name}</CardTitle>
          <SharedBadge pe={pe} />
        </div>
        <div className="flex items-center gap-3 font-normal">
          {clock && <span className="text-xs text-subtle">{clock}</span>}
          {daysLeft != null && daysLeft > 0 && (
            <span className={clsx('text-2xs', daysLeft < 2 ? 'text-danger' : 'text-muted')}>{daysLeft}d remaining</span>
          )}
          {!readOnly && <Button size="sm" onClick={e => { e.stopPropagation(); onEdit(pe) }}>Edit</Button>}
        </div>
      </CardHeader>

      <CardContent onClick={() => router.push(href)}>
        {pe.description && <CollapsibleDescription text={pe.description} className="mb-3" />}

        {pe.metrics && pe.metrics.length > 0 && (
          <div className="mb-3.5 flex flex-wrap gap-1.5">
            {pe.metrics.map((m, i) => <MetricChip key={m.key} m={m} primary={i === 0} />)}
          </div>
        )}

        <div className="grid grid-cols-4 gap-4">
          <div>
            <Eyebrow className="block">Budget</Eyebrow>
            <div className="font-mono text-xl font-medium">{pe.budget_accelerator_hours} <span className="text-xs text-muted">{unit}</span></div>
          </div>
          <div>
            <Eyebrow className="block">Agents</Eyebrow>
            <div className="font-mono text-xl font-medium">{pe.signup_count} <span className="text-xs text-muted">joined, {pe.max_agents} researchers at most</span></div>
          </div>
          <div>
            <Eyebrow className="block">Budget Used</Eyebrow>
            <div className={'font-mono text-xl font-medium'}>
              {formatAccH(totalUsed)} <span className="text-xs text-muted">{unit}</span>
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

      </CardContent>
    </Card>
  )
}
