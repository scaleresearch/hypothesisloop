'use client'

import useSWR from 'swr'
import { clsx } from 'clsx'
import { fetchHypothesisStats } from '@/lib/api/hypotheses'
import { fetchPlatformExperimentQuotas } from '@/lib/api/quotas'
import type { Draft } from '@/types/planning'
import { PlatformExperimentStatus } from '@/types/platform-experiments'
import type { PlatformExperiment } from '@/types/platform-experiments'
import type { AgentQuota } from '@/types/quotas'
import { useRouter } from 'next/navigation'
import { experimentPath } from '@/lib/routes'
import { isReadOnly } from '@/lib/read-only'
import { formatAccH, isZeroDate } from '@/lib/format'
import { budgetKind, budgetUnit } from '@/lib/units'
import { useTenant } from '@/lib/tenant'
import { Card } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import { SharedBadge } from '@/components/shared-badge'
import { Button } from '@/components/ui/button'
import { Inline } from '@/components/ui/stack'
import { RemoveExperiment } from '@/components/remove-experiment'
import { keys } from '@/lib/keys'
import { POLL } from '@/lib/poll'

const day = (iso: string) => new Date(iso).toLocaleDateString(undefined, { dateStyle: 'medium' })

// The API takes an edit of an open or a running experiment only.
const EDITABLE: string[] = [PlatformExperimentStatus.OPEN, PlatformExperimentStatus.RUNNING]

/**
 * One platform experiment in a list, on one line, opening its page. It shows the figures its
 * state gives a meaning to and none that are zero. One the caller only reads offers no edit and
 * no removal.
 */
export function PlatformExperimentCard({
  pe,
  onEdit,
}: {
  pe: PlatformExperiment
  onEdit: (pe: PlatformExperiment) => void
}) {
  const router = useRouter()
  const { me } = useTenant()
  const readOnly = isReadOnly(me, pe)
  const href = experimentPath(pe.id)
  const started = pe.status !== PlatformExperimentStatus.DRAFT

  const { data: quotas, error: quotasError } = useSWR<AgentQuota[]>(
    started ? keys.quotas(pe.id) : null,
    () => fetchPlatformExperimentQuotas(pe.id),
    { refreshInterval: POLL.reports },
  )
  const { data: verdicts, error: verdictsError } = useSWR(
    started ? keys.hypothesisStats(pe.id) : null,
    () => fetchHypothesisStats(pe.id),
    { refreshInterval: POLL.standing },
  )

  const unit = budgetUnit(budgetKind((pe as Draft).proposal?.accelerator_type, []))
  const live = pe.status === PlatformExperimentStatus.OPEN || pe.status === PlatformExperimentStatus.RUNNING
  const daysLeft = live && !isZeroDate(pe.ends_at)
    ? Math.ceil((new Date(pe.ends_at!).getTime() - Date.now()) / 86_400_000)
    : null

  // A figure whose read failed says so, and one still in flight is left out, so neither reads as none.
  const figures: { text: string; title?: string }[] = []
  if (quotasError) {
    figures.push({ text: 'compute used unknown', title: `Could not load: ${quotasError.message}` })
  } else if (!started || quotas) {
    const used = (quotas ?? []).reduce((s, q) => s + q.used_guaranteed_acch + q.used_burst_acch, 0)
    if (live && pe.budget_accelerator_hours > 0) {
      figures.push({ text: used > 0 ? `${formatAccH(used)} of ${pe.budget_accelerator_hours} ${unit}` : `${pe.budget_accelerator_hours} ${unit}` })
    } else if (used > 0) {
      figures.push({ text: `${formatAccH(used)} ${unit} used` })
    }
  }
  if (live && pe.signup_count > 0) figures.push({ text: `${pe.signup_count} ${pe.signup_count === 1 ? 'agent' : 'agents'}` })
  if (verdictsError) {
    figures.push({ text: 'hypotheses unknown', title: `Could not load: ${verdictsError.message}` })
  } else if (verdicts && verdicts.total > 0) {
    figures.push({ text: `${verdicts.total} ${verdicts.total === 1 ? 'hypothesis' : 'hypotheses'}` })
    for (const v of ['confirmed', 'refuted'] as const) {
      const n = verdicts.by_status[v] ?? 0
      if (n > 0) figures.push({ text: `${n} ${v}` })
    }
  }

  const when = pe.status === PlatformExperimentStatus.CLOSED && !isZeroDate(pe.closed_at)
    ? day(pe.closed_at!)
    : !live && !isZeroDate(pe.created_at) ? day(pe.created_at) : null

  return (
    <Card interactive>
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
        <Inline gap="lg" nowrap className="ml-auto shrink-0 text-xs tabular-nums text-muted">
          {figures.map(f => <span key={f.text} title={f.title} className={clsx(f.title && 'text-danger')}>{f.text}</span>)}
          {daysLeft != null && daysLeft > 0 && <span className={clsx(daysLeft < 2 && 'text-danger')}>{daysLeft}d left</span>}
          {when && <span>{when}</span>}
          {!readOnly && EDITABLE.includes(pe.status) && (
            <Button size="sm" onClick={e => { e.stopPropagation(); onEdit(pe) }}>Edit</Button>
          )}
          {!readOnly && (
            // The dialog is rendered in a portal, and its clicks and keys still bubble to the row through React.
            <span onClick={e => e.stopPropagation()} onKeyDown={e => e.stopPropagation()}>
              <RemoveExperiment pe={pe} label="Remove" />
            </span>
          )}
        </Inline>
      </div>
    </Card>
  )
}
