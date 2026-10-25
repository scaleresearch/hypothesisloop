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

// A long id as its kind and its last characters: ids are time-ordered, so their starts repeat
// across a list and their ends tell records apart. The full id goes in a title.
export function shortId(id: string): string {
  const kind = /^[a-z]+-/.exec(id)?.[0] ?? ''
  const rest = id.slice(kind.length)
  return rest.length <= 8 ? id : `${kind}…${rest.slice(-6)}`
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

// A priced type for a person when the catalog gives it no label: the product after the "=" of its
// selector, without the NVIDIA prefix ("NVIDIA-A100-80GB-PCIe" reads "A100 80GB PCIe",
// "AMD_Instinct_MI300X" "AMD Instinct MI300X"). CPU cores and Tenstorrent chips say what they are.
export function resourceLabel(type: string): string {
  if (type === 'hypothesisloop.dev/cpu=core') return 'CPU core'
  const name = acceleratorLabel(type)
  if (type.startsWith('tenstorrent.com/')) return `Tenstorrent ${name.charAt(0).toUpperCase()}${name.slice(1)}`
  return name.replace(/^NVIDIA-(GeForce-)?/, '').replace(/[-_]/g, ' ')
}

export function formatDuration(ms: number): string {
  const s = Math.round(ms / 1000)
  if (s < 60) return `${s} s`
  if (s < 3600) return `${Math.floor(s / 60)} min ${s % 60} s`
  return `${Math.floor(s / 3600)} h ${Math.floor((s % 3600) / 60)} min`
}

// A long text as a label: its first sentence, cut at a word near max characters. A sentence's
// closing full stop goes, so the label can sit inside a sentence of its own.
export function headline(text: string, max = 120): string {
  const t = text.trim().replace(/^(mechanism|claim|hypothesis|idea):\s*/i, '')
  const first = (/^[\s\S]*?[.!?](?=\s|$)/.exec(t)?.[0] ?? t).replace(/\.$/, '')
  if (first.length <= max) return first
  const space = first.lastIndexOf(' ', max)
  return `${first.slice(0, space > max / 2 ? space : max).replace(/[\s,;:(–-]+$/, '')}…`
}

// Every metric value the UI shows, in a table, a sentence or a chart's tooltip: four significant
// digits, so 0.37109421 reads 0.3711 and 1234.56 reads 1235.
export function formatMetric(v: number): string {
  if (!Number.isFinite(v)) return String(v)
  if (v === 0) return '0'
  const a = Math.abs(v)
  if (a >= 1e4) return Math.round(v).toLocaleString('en-US')
  if (a < 1e-3) return v.toExponential(2)
  return v.toPrecision(4)
}
