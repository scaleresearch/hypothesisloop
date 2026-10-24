import { fontWeight, textPx } from '@/lib/scale'

const token = (name: string) => `rgb(var(--${name}))`

/** The theme tokens of globals.css as colour values, for charts, graph strokes and inline status colours. */
export const semantic = {
  success: token('success'),
  warning: token('warning'),
  danger: token('danger'),
  info: token('info'),
  accent: token('accent'),
  primary: token('primary'),
  neutral: token('neutral'),
  text: token('text'),
  muted: token('text-muted'),
  subtle: token('text-subtle'),
  border: token('border'),
  borderStrong: token('border-strong'),
  borderControl: token('border-control'),
  surface: token('surface'),
  surfaceInset: token('surface-inset'),
  surfaceRaised: token('surface-raised'),
} as const

/** Colours a brand prescribes for its own mark, which no theme token may stand in for. */
export const brand = {
  google: { blue: '#4285F4', green: '#34A853', yellow: '#FBBC05', red: '#EA4335' },
} as const

/** The states of globals.css a colour can be laid on at, the same as the class tints of tailwind.config.ts. */
type TintLevel = 'hover' | 'track' | 'tint' | 'faint' | 'edge'

/** A colour at one of the theme's state strengths, for a fill behind a line or dot of the same colour. */
export function tint(color: string, level: TintLevel): string {
  return `color-mix(in srgb, ${color} calc(var(--alpha-${level}) * 100%), transparent)`
}

export type Tone = 'success' | 'warning' | 'danger' | 'info' | 'accent' | 'neutral'

// Job, platform experiment, capacity tier, hypothesis and participant states, lowercased.
// success, warning and danger say how a thing is going and nothing else: running or confirmed,
// waiting, failed or refuted. What is only a kind stays neutral; its label tells it apart.
const tones: Record<string, Tone> = {
  running: 'success',
  completed: 'info',
  queued: 'warning',
  failed: 'danger',
  evicted: 'warning',
  admitted: 'info',
  submitted: 'neutral',
  promoted: 'success',
  rejected: 'danger',
  cancelled: 'neutral',
  draft: 'neutral',
  open: 'info',
  closed: 'neutral',
  guaranteed: 'neutral',
  burst: 'neutral',
  confirmed: 'success',
  refuted: 'danger',
  inconclusive: 'neutral',
  merged: 'neutral',
  retired: 'neutral',
  human: 'neutral',
  agent: 'neutral',
}

export function statusTone(status?: string | null): Tone {
  return tones[(status ?? '').toLowerCase()] ?? 'neutral'
}

export function statusColor(status?: string | null): string {
  return semantic[statusTone(status)]
}

/** The ordered series palette for charts. Assign in order; the order keeps neighbours apart for colour-blind readers. */
export const seriesPalette = [1, 2, 3, 4, 5, 6, 7, 8].map(n => token(`chart-${n}`))


/**
 * Hypothesis graph edges. builds_on is the hierarchy; a cross link carries its label on the edge
 * and stays solid: a dashed stroke is generated along the whole path, offscreen parts included,
 * and long cross links in a graph of thousands made every pan frame take seconds.
 * builds_on is drawn in the control edge colour, the lightest grey that holds 3:1 on the canvas,
 * and labelled in muted text, as no border grey can carry text.
 */
export const hypothesisLinkStyles = {
  builds_on: { color: semantic.borderControl, text: semantic.muted },
  contradicts: { color: semantic.danger, text: semantic.danger },
  related: { color: semantic.info, text: semantic.info },
} as const

/** Props every recharts chart shares, so axes, grid, labels and tooltip look the same on each. */
export const chart = {
  grid: semantic.border,
  tick: { fontSize: textPx['2xs'], fill: semantic.muted },
  tooltip: {
    contentStyle: {
      background: semantic.surfaceRaised,
      border: `1px solid ${semantic.border}`,
      borderRadius: 'var(--radius)',
      boxShadow: 'var(--shadow-md)',
      padding: '0.5rem 0.75rem',
      fontSize: textPx.xs,
    },
    labelStyle: { color: semantic.text, fontWeight: fontWeight.medium },
    cursor: { fill: tint(semantic.text, 'hover') },
  },
  legend: { fontSize: textPx['2xs'] },
  /** A label drawn on a line or an edge. */
  label: { fontSize: textPx['2xs'], fontWeight: fontWeight.medium },
} as const
