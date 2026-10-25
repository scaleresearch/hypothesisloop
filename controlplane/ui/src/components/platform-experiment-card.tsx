'use client'

import useSWR from 'swr'
import { clsx } from 'clsx'
import { fetchHypothesisStats } from '@/lib/api/hypotheses'
import { useRouter } from 'next/navigation'
import { experimentPath } from '@/lib/routes'
import { formatAccH, isZeroDate } from '@/lib/format'
import { budgetUnit } from '@/lib/units'
import { useBudgetReference } from '@/lib/capacity'
import { Card } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import { SharedBadge } from '@/components/shared-badge'
import { Button } from '@/components/ui/button'
import { Inline } from '@/components/ui/stack'
import { RemoveExperiment } from '@/components/remove-experiment'
import { keys } from '@/lib/keys'
import type { PlatformExperiment } from '@/types/api'

const day = (iso: string) => new Date(iso).toLocaleDateString(undefined, { dateStyle: 'medium' })

/**
 * One platform experiment in a list, on one line, opening its page. It shows the figures its
 * state gives a meaning to and none that are zero. It offers the edit and the removal the API lists
 * for the caller.
 */
export function PlatformExperimentCard({
  pe,
  onEdit,
}: {
  pe: PlatformExperiment
  onEdit: (pe: PlatformExperiment) => void
}) {
  const reference = useBudgetReference()
  const router = useRouter()
  const href = experimentPath(pe.id)
  const started = pe.status !== 'draft'

  const { data: verdicts, error: verdictsError } = useSWR(
    started ? keys.hypothesisStats(pe.id) : null,
    () => fetchHypothesisStats(pe.id),
  )

  const unit = budgetUnit(pe.budget_kind, 'short', reference)
  const live = pe.status === 'open' || pe.status === 'running'
  const daysLeft = live && !isZeroDate(pe.ends_at)
    ? Math.ceil((new Date(pe.ends_at!).getTime() - Date.now()) / 86_400_000)
    : null

  // A figure whose read failed says so, and one still in flight is left out, so neither reads as none.
  const figures: { text: string; title?: string }[] = []
  const used = pe.consumed_accelerator_hours ?? 0
  if (live && pe.budget_accelerator_hours > 0) {
    figures.push({ text: used > 0 ? `${formatAccH(used)} of ${pe.budget_accelerator_hours} ${unit}` : `${pe.budget_accelerator_hours} ${unit}` })
  } else if (used > 0) {
    figures.push({ text: `${formatAccH(used)} ${unit} spent` })
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

  const when = pe.status === 'closed' && !isZeroDate(pe.closed_at)
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
          {pe.editable.some(field => field !== 'goal') && (
            <Button size="sm" onClick={e => { e.stopPropagation(); onEdit(pe) }}>Edit</Button>
          )}
          {pe.actions.includes('remove') && (
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
