const token = (name: string) => `rgb(var(--${name}))`

/** The theme tokens of globals.css as colour values, for charts, graph strokes and inline status colours. */
export const semantic = {
  success: token('success'),
  warning: token('warning'),
  danger: token('danger'),
  info: token('info'),
  accent: token('accent'),
  primary: token('primary'),
  purple: token('purple'),
  pink: token('pink'),
  orange: token('orange'),
  neutral: token('neutral'),
  text: token('text'),
  muted: token('text-muted'),
  subtle: token('text-subtle'),
  border: token('border'),
  borderStrong: token('border-strong'),
  surface: token('surface'),
  surfaceInset: token('surface-inset'),
  surfaceRaised: token('surface-raised'),
} as const

/** Colours a brand prescribes for its own mark, which no theme token may stand in for. */
export const brand = {
  google: { blue: '#4285F4', green: '#34A853', yellow: '#FBBC05', red: '#EA4335' },
} as const

/** A colour at the given opacity (0 to 1), for fills behind a line or dot of the same colour. */
export function tint(color: string, opacity: number): string {
  return `color-mix(in srgb, ${color} ${Math.round(opacity * 100)}%, transparent)`
}

export type Tone = 'success' | 'warning' | 'danger' | 'info' | 'accent' | 'purple' | 'pink' | 'neutral'

// Job, platform experiment, capacity tier, hypothesis and participant states, lowercased.
const tones: Record<string, Tone> = {
  running: 'success',
  completed: 'accent',
  queued: 'warning',
  failed: 'danger',
  evicted: 'purple',
  admitted: 'info',
  submitted: 'neutral',
  promoted: 'pink',
  rejected: 'danger',
  cancelled: 'neutral',
  draft: 'neutral',
  open: 'accent',
  closed: 'neutral',
  guaranteed: 'accent',
  burst: 'purple',
  confirmed: 'success',
  refuted: 'danger',
  inconclusive: 'neutral',
  human: 'warning',
  agent: 'neutral',
}

export function statusTone(status?: string | null): Tone {
  return tones[(status ?? '').toLowerCase()] ?? 'neutral'
}

export function statusColor(status?: string | null): string {
  return semantic[statusTone(status)]
}

/** Categorical palette for per-agent series in charts (recharts Line/Cell colors). */
export const agentPalette = [
  semantic.accent,
  semantic.warning,
  semantic.success,
  semantic.pink,
  semantic.danger,
  semantic.info,
  semantic.orange,
  semantic.purple,
]

export const hypothesisStatuses = ['open', 'confirmed', 'refuted', 'inconclusive'] as const

/**
 * Hypothesis graph edges. builds_on is the hierarchy; a cross link carries its label on the edge
 * and stays solid: a dashed stroke is generated along the whole path, offscreen parts included,
 * and long cross links in a graph of thousands made every pan frame take seconds.
 */
export const hypothesisLinkStyles = {
  builds_on: { color: semantic.borderStrong, label: 'builds on' },
  contradicts: { color: semantic.danger, label: 'contradicts' },
  related: { color: semantic.info, label: 'related' },
} as const

/** Props every recharts chart shares, so axes, grid and tooltip look the same on each. */
export const chart = {
  grid: semantic.border,
  tick: { fontSize: 10, fill: semantic.muted },
  tooltip: {
    contentStyle: {
      background: semantic.surfaceRaised,
      border: `1px solid ${semantic.borderStrong}`,
      borderRadius: 'var(--radius)',
      fontSize: 12,
    },
    labelStyle: { color: semantic.text },
    cursor: { fill: tint(semantic.text, 0.04) },
  },
  legend: { fontSize: 10 },
} as const
