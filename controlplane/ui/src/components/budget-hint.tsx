'use client'

import { useCapacity } from '@/lib/capacity'
import { acceleratorLabel } from '@/lib/format'
import { budgetUnit } from '@/lib/units'
import { InfoTip } from '@/components/ui/info-tip'
import type { LiveCapacity, TypeKind } from '@/types/api'

export interface Rate {
  type: string
  label: string
  rate: number
}

/** The accelerator types the org's jobs can be placed on, each once with what an hour on it counts, dearest first. */
export function acceleratorRates(capacity: LiveCapacity | undefined): Rate[] {
  const rates = new Map<string, number>()
  for (const c of capacity?.clusters ?? []) {
    for (const a of c.accelerators) {
      if (a.kind === 'accelerator' && a.acch_per_hour) rates.set(a.accelerator_type, a.acch_per_hour)
    }
  }
  return Array.from(rates, ([type, rate]) => ({ type, label: acceleratorLabel(type), rate }))
    .sort((a, b) => b.rate - a.rate || a.label.localeCompare(b.label))
}

const round = (n: number) => String(Math.round(n * 10) / 10)

/** What a budget buys on one type, in words: "About 40 hours on RTX 4090, which counts 0.25 per hour." */
export function budgetOn(budget: number, rate: Rate): string {
  return `About ${round(budget / rate.rate)} hours on ${rate.label}, which counts ${rate.rate} per hour.`
}

/** The InfoTip of a budget: what it is counted in and the rate of each type available. */
export function budgetExplained(kind: TypeKind | undefined, reference: string | undefined, rates: Rate[]): string {
  if (kind === 'cpu') return `Counted in ${budgetUnit('cpu', 'long')}: a core for an hour counts 1. Memory and storage spend nothing.`
  const listed = rates.length > 0 ? `: ${rates.map(r => `${r.label} ${r.rate}`).join(', ')}` : ''
  const cpu = kind === undefined ? ` Jobs on CPU count ${budgetUnit('cpu', 'long')}.` : ''
  return `Budgets are counted in ${budgetUnit('accelerator', 'long', reference)}. An hour on another type counts at its rate${listed}.${cpu} Memory and storage spend nothing.`
}

/** The hint of a budget field: what it buys on acceleratorType as it is typed, and the full explanation in an InfoTip. */
export function BudgetHint({ budget, kind, acceleratorType }: { budget: string; kind?: TypeKind; acceleratorType?: string }) {
  const { data } = useCapacity()
  const rates = acceleratorRates(data)
  const rate = acceleratorType ? rates.find(r => r.type === acceleratorType) : undefined
  const n = Number(budget)
  return (
    <>
      {rate && budget.trim() !== '' && n > 0 && budgetOn(n, rate)}
      <InfoTip>{budgetExplained(kind, data?.budget_reference || undefined, rates)}</InfoTip>
    </>
  )
}
