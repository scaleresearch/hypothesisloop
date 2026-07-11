// Formats a T4h (GPU-hour) quantity for display. Values ≥ 1 round to the nearest
// half-hour (readable at normal scale); smaller values keep 3 decimals instead of
// being rounded away to "0" — small demo/test workloads consume fractions of an hour.
export function formatT4h(n: number): string {
  if (n === 0) return '0'
  if (Math.abs(n) < 1) return n.toFixed(3)
  return String(Math.round(n * 2) / 2)
}
