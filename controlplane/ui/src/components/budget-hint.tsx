'use client'

import { useCapacity } from '@/lib/capacity'
import { acceleratorLabel } from '@/lib/format'
import { orgPath } from '@/lib/routes'
import { useTenant } from '@/lib/tenant'
import { budgetUnit } from '@/lib/units'
import { InfoTip } from '@/components/ui/info-tip'
import { TextLink } from '@/components/ui/link'
import type { LiveCapacity, TypeKind } from '@/types/api'

export interface Rate {
  type: string
  label: string
  rate: number
}

/** The accelerator types the org's jobs can be placed on, each once with what an hour on it counts, dearest first. */
export function acceleratorRates(capacity: LiveCapacity | undefined): Rate[] {
  const rates = new Map<string, Rate>()
  for (const c of capacity?.clusters ?? []) {
    for (const a of c.accelerators) {
      if (a.kind === 'accelerator' && a.acch_per_hour) {
        rates.set(a.accelerator_type, { type: a.accelerator_type, label: a.label || acceleratorLabel(a.accelerator_type), rate: a.acch_per_hour })
      }
    }
  }
  return Array.from(rates.values())
    .sort((a, b) => b.rate - a.rate || a.label.localeCompare(b.label))
}

const round = (n: number) => String(Math.round(n * 10) / 10)

/** What a budget buys on one type, in words: "About 40 hours on RTX 4090, which counts 0.25 per hour." */
export function budgetOn(budget: number, rate: Rate): string {
  return `About ${round(budget / rate.rate)} hours on ${rate.label}, which counts ${rate.rate} per hour.`
}

/** The InfoTip of a budget: what it is counted in. The rate of each type is on the Compute page. */
export function budgetExplained(kind: TypeKind | undefined, reference: string | undefined): string {
  if (kind === 'cpu') return `Counted in ${budgetUnit('cpu', 'long')}: a core for an hour counts 1. Memory and storage spend nothing.`
  const cpu = kind === undefined ? ` Jobs on CPU count ${budgetUnit('cpu', 'long')}.` : ''
  return `Budgets are counted in ${budgetUnit('accelerator', 'long', reference)}. An hour on another type counts at its rate.${cpu} Memory and storage spend nothing.`
}

/** The hint of a budget field: what it buys on acceleratorType as it is typed, the explanation in an InfoTip and a link to the rates. */
export function BudgetHint({ budget, kind, acceleratorType }: { budget: string; kind?: TypeKind; acceleratorType?: string }) {
  const { org } = useTenant()
  const { data } = useCapacity()
  const rates = acceleratorRates(data)
  const rate = acceleratorType ? rates.find(r => r.type === acceleratorType) : undefined
  const n = Number(budget)
  return (
    <>
      {rate && budget.trim() !== '' && n > 0 && budgetOn(n, rate)}
      <InfoTip>{budgetExplained(kind, data?.budget_reference || undefined)}</InfoTip>
      {kind !== 'cpu' && <> <TextLink inline href={`${orgPath(org.id, 'clusters')}#rates`}>Rates</TextLink></>}
    </>
  )
}
