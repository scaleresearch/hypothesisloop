import type { TypeKind } from '@/types/api'

// A priced type counts devices or, for the one CPU type, cores, as the API's accelerator_kind
// says. Counts of the two are shown apart and a count of cores says so.

// The unit a budget, what is used of it and what is left are read in. A core for an hour costs
// 1, so a figure spent on cores is its CPU core-hours. An accelerator-hour is an hour on the
// reference accelerator the platform names (useBudgetReference), H100-hours where that is H100.
export function budgetUnit(kind: TypeKind | undefined, form: 'short' | 'long' = 'short', reference?: string): string {
  if (kind === 'cpu') return form === 'long' ? 'CPU core-hours' : 'CPU hours'
  if (reference) return form === 'long' ? `${reference}-hours` : `${reference} h`
  return form === 'long' ? 'accelerator-hours' : 'AccH'
}

export function unitCount(kind: TypeKind | undefined, n: number): string {
  if (kind !== 'cpu') return String(n)
  return `${n} ${n === 1 ? 'core' : 'cores'}`
}

// What one unit of the type costs for an hour, in the unit budgets are counted in.
export function unitRate(kind: TypeKind | undefined, acchPerHour: number, reference?: string): string {
  const n = Math.round(acchPerHour * 1000) / 1000
  if (kind === 'cpu') return `${n} CPU-${n === 1 ? 'hour' : 'hours'}`
  const unit = budgetUnit('accelerator', 'long', reference)
  return `${n} ${n === 1 ? unit.replace(/s$/, '') : unit}`
}

// Devices and cores in flight, as one value. The concurrency cap counts the two together.
export function inFlight(accelerators: number, cores: number, cap: number | null | undefined): { label: string; value: string; atCap: boolean } {
  const held = cores > 0 ? `${accelerators} accelerators, ${unitCount('cpu', cores)}` : String(accelerators)
  return {
    label: cores > 0 ? 'Accelerators And Cores In Flight' : 'Accelerators In Flight',
    value: cap == null ? held : `${held} / ${cap}`,
    atCap: cap != null && accelerators + cores >= cap,
  }
}

export function occupancy(busy: number, total: number, unit: string): { percent: string; detail: string | null } {
  if (total <= 0) return { percent: '—', detail: null }
  return { percent: `${Math.round((busy / total) * 100)}%`, detail: `${busy}/${total} ${unit} busy, live across connected clusters` }
}
