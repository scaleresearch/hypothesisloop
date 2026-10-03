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
  success: 'border-success-edge bg-success-faint',
  warning: 'border-warning-edge bg-warning-faint',
  danger: 'border-danger-edge bg-danger-faint',
  info: 'border-info-edge bg-info-faint',
  accent: 'border-accent-edge bg-accent-faint',
  neutral: 'border-border bg-surface-inset',
}
