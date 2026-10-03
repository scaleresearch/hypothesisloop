'use client'

import useSWR from 'swr'
import { clsx } from 'clsx'
import { fetchPlatformExperimentQuotas, fetchHypothesisStats } from '@/lib/api'
import { PlatformExperimentStatus } from '@/types'
import type { PlatformExperiment, AgentQuota } from '@/types'
import { useProjectRouter } from '@/lib/query-state'
import { isReadOnly } from '@/lib/read-only'
import { formatAccH, isZeroDate } from '@/lib/format'
import { budgetKind, budgetUnit } from '@/lib/units'
import type { Draft } from '@/lib/planning'
import { useTenant } from '@/lib/tenant'
import { Card } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import { SharedBadge } from '@/components/shared-badge'
import { Button } from '@/components/ui/button'

const day = (iso: string) => new Date(iso).toLocaleDateString(undefined, { dateStyle: 'medium' })

// The API takes an edit of an open or a running experiment only.
const EDITABLE: string[] = [PlatformExperimentStatus.OPEN, PlatformExperimentStatus.RUNNING]

/**
 * One platform experiment in a list, on one line, opening its page. It shows the figures its
 * state gives a meaning to and none that are zero. One the caller only reads offers no edit.
 */
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
  const started = pe.status !== PlatformExperimentStatus.DRAFT

  const { data: quotas } = useSWR<AgentQuota[]>(
    started ? ['pe-quotas', pe.id] : null,
    () => fetchPlatformExperimentQuotas(pe.id),
    { refreshInterval: 10_000 },
  )
  const { data: verdicts } = useSWR(
    started ? ['pe-hypothesis-stats', pe.id] : null,
    () => fetchHypothesisStats(pe.id),
    { refreshInterval: 15_000 },
  )

  const used = (quotas ?? []).reduce((s, q) => s + q.used_guaranteed_acch + q.used_burst_acch, 0)
  const unit = budgetUnit(budgetKind((pe as Draft).proposal?.accelerator_type, []))
  const live = pe.status === PlatformExperimentStatus.OPEN || pe.status === PlatformExperimentStatus.RUNNING
  const daysLeft = live && !isZeroDate(pe.ends_at)
    ? Math.ceil((new Date(pe.ends_at!).getTime() - Date.now()) / 86_400_000)
    : null

  const figures: string[] = []
  if (live && pe.budget_accelerator_hours > 0) {
    figures.push(used > 0 ? `${formatAccH(used)} of ${pe.budget_accelerator_hours} ${unit}` : `${pe.budget_accelerator_hours} ${unit}`)
  } else if (used > 0) {
    figures.push(`${formatAccH(used)} ${unit} used`)
  }
  if (live && pe.signup_count > 0) figures.push(`${pe.signup_count} ${pe.signup_count === 1 ? 'agent' : 'agents'}`)
  const hypotheses = verdicts?.total ?? 0
  if (hypotheses > 0) {
    figures.push(`${hypotheses} ${hypotheses === 1 ? 'hypothesis' : 'hypotheses'}`)
    for (const v of ['confirmed', 'refuted'] as const) {
      const n = verdicts?.by_status[v] ?? 0
      if (n > 0) figures.push(`${n} ${v}`)
    }
  }

  const when = pe.status === PlatformExperimentStatus.CLOSED && !isZeroDate(pe.closed_at)
    ? day(pe.closed_at!)
    : !live && !isZeroDate(pe.created_at) ? day(pe.created_at) : null

  return (
    <Card className="mb-2.5 cursor-pointer transition hover:border-border-strong hover:shadow-md">
      <div
        className="flex min-h-14 items-center gap-4 px-5 py-2.5"
        onClick={() => router.push(href)}
        tabIndex={0}
        role="link"
        onKeyDown={e => { if (e.key === 'Enter') router.push(href) }}
      >
        <span className="w-20 flex-none"><Badge status={pe.status}>{pe.status}</Badge></span>
        <span className="min-w-0 truncate text-base text-foreground">{pe.name}</span>
        <SharedBadge pe={pe} />
        <span className="ml-auto flex shrink-0 items-center gap-4 text-xs tabular-nums text-muted">
          {figures.map(f => <span key={f}>{f}</span>)}
          {daysLeft != null && daysLeft > 0 && <span className={clsx(daysLeft < 2 && 'text-danger')}>{daysLeft}d left</span>}
          {when && <span>{when}</span>}
          {!readOnly && EDITABLE.includes(pe.status) && (
            <Button size="sm" onClick={e => { e.stopPropagation(); onEdit(pe) }}>Edit</Button>
          )}
        </span>
      </div>
    </Card>
  )
}
