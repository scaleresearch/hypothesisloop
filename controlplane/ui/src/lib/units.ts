import type { TypeKind } from '@/types/api'

// A priced type counts devices or, for the one CPU type, cores. Counts of the two are shown apart
// and a count of cores says so.

// The type every runtime offers its cores as (domain.CPUType). A job that states cpu and no
// accelerator holds that many units of it.
export const CPU_TYPE = 'hypothesisloop.dev/cpu=core'

export function kindOf(acceleratorType: string | undefined): TypeKind {
  return acceleratorType?.toLowerCase() === CPU_TYPE ? 'cpu' : 'accelerator'
}

// What an experiment's budget is spent on: the type its proposal was sized for, and with no
// proposal cores only when it has jobs and every one of them runs on cores.
export function budgetKind(proposedType: string | undefined, jobTypes: string[]): TypeKind {
  if (proposedType) return kindOf(proposedType)
  return jobTypes.length > 0 && jobTypes.every(type => kindOf(type) === 'cpu') ? 'cpu' : 'accelerator'
}

// The unit a budget, what is used of it and what is left are read in. A core for an hour costs
// 1, so a figure spent on cores is its CPU core-hours.
export function budgetUnit(kind: TypeKind | undefined, form: 'short' | 'long' = 'short'): string {
  if (kind === 'cpu') return form === 'long' ? 'CPU core-hours' : 'CPU hours'
  return form === 'long' ? 'accelerator-hours' : 'AccH'
}

export function unitCount(kind: TypeKind | undefined, n: number): string {
  if (kind !== 'cpu') return String(n)
  return `${n} ${n === 1 ? 'core' : 'cores'}`
}

// What one unit of the type costs for an hour, in accelerator hours.
export function unitRate(kind: TypeKind | undefined, acchPerHour: number): string {
  return kind === 'cpu' ? `${acchPerHour} per core-hour` : String(acchPerHour)
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
