import type { Tone } from '@/lib/colors'

export const toneText: Record<Tone, string> = {
  success: 'text-success',
  warning: 'text-warning',
  danger: 'text-danger',
  info: 'text-info',
  accent: 'text-accent',
  neutral: 'text-neutral',
}

/** A faint fill and edge of the tone, behind text of the same tone or of the default colour. */
export const toneTint: Record<Tone, string> = {
  success: 'border-success/25 bg-success/5',
  warning: 'border-warning/25 bg-warning/5',
  danger: 'border-danger/25 bg-danger/5',
  info: 'border-info/25 bg-info/5',
  accent: 'border-accent/25 bg-accent/5',
  neutral: 'border-border bg-surface-inset',
}
