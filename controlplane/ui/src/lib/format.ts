// Go's zero time.Time value ("never set") marshals to this JSON string. A `not null`
// timestamptz column with no application-level default stores it literally, so it round-trips
// as a syntactically valid but meaningless date — must be treated as unset, not formatted.
const ZERO_TIME_PREFIX = '0001-01-01'

export function isZeroDate(iso: string | null | undefined): boolean {
  return !iso || iso.startsWith(ZERO_TIME_PREFIX)
}

// Formats an ISO timestamp as date and time, or 'N/A' if it's absent or the Go zero-time value.
export function formatDate(iso: string | null | undefined): string {
  return isZeroDate(iso) ? 'N/A' : new Date(iso as string).toLocaleString(undefined, { dateStyle: 'short', timeStyle: 'short' })
}

// The clock of a platform experiment: how long an open one may wait for its start, and from when
// to when a started one runs. Null when it has neither, as a draft.
export function runClock(pe: { started_at?: string; ends_at?: string; baseline_due_at?: string }): string | null {
  if (pe.started_at) return `${formatDate(pe.started_at)} – ${formatDate(pe.ends_at)}`
  if (pe.baseline_due_at) return `open until ${formatDate(pe.baseline_due_at)}`
  return null
}

// The start of a long id, enough to tell records apart in a list; the full id goes in a title.
export function shortId(id: string): string {
  return `${id.slice(0, 8)}…`
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

// A node label selector names the accelerator after its "=" ("nvidia.com/gpu.product=NVIDIA-L40");
// tables show that product name and keep the full selector for the job's own page.
export function acceleratorLabel(type: string): string {
  return type.slice(type.lastIndexOf('=') + 1)
}

export function formatDuration(ms: number): string {
  const s = Math.round(ms / 1000)
  if (s < 60) return `${s} s`
  if (s < 3600) return `${Math.floor(s / 60)} min ${s % 60} s`
  return `${Math.floor(s / 3600)} h ${Math.floor((s % 3600) / 60)} min`
}
