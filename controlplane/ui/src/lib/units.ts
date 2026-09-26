// A priced type counts devices or, for a CPU class, cores (the catalog's kind). Counts of the two
// are shown apart and a count of cores says so.
export type TypeKind = 'accelerator' | 'cpu'

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
