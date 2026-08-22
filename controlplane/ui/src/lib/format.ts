// Go's zero time.Time value ("never set") marshals to this JSON string. A `not null`
// timestamptz column with no application-level default stores it literally, so it round-trips
// as a syntactically valid but meaningless date — must be treated as unset, not formatted.
const ZERO_TIME_PREFIX = '0001-01-01'

export function isZeroDate(iso: string | null | undefined): boolean {
  return !iso || iso.startsWith(ZERO_TIME_PREFIX)
}

// Formats an ISO timestamp for display, or 'N/A' if it's absent or the Go zero-time value.
export function formatDate(iso: string | null | undefined): string {
  return isZeroDate(iso) ? 'N/A' : new Date(iso as string).toLocaleDateString()
}

// Formats a AccH (Accelerator-hour) quantity for display. Values ≥ 1 round to the nearest
// half-hour (readable at normal scale); smaller values keep 3 decimals instead of
// being rounded away to "0" — small demo/test workloads consume fractions of an hour.
export function formatAccH(n: number): string {
  if (n === 0) return '0'
  if (Math.abs(n) < 1) return n.toFixed(3)
  return String(Math.round(n * 2) / 2)
}

// Relative age for a timestamp, falling back to an absolute date past a day. Zero-time-aware:
// an unset Go timestamp is "N/A", not "2025y ago" — which is what the three copied local
// versions of this each rendered.
export function relTime(iso: string | null | undefined): string {
  if (isZeroDate(iso)) return 'N/A'
  const then = new Date(iso as string)
  const s = Math.round((Date.now() - then.getTime()) / 1000)
  if (s < 60) return `${s}s ago`
  if (s < 3600) return `${Math.round(s / 60)}m ago`
  if (s < 86400) return `${Math.round(s / 3600)}h ago`
  return then.toLocaleDateString()
}
