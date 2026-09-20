import type { Tone } from '@/lib/colors'

export const toneText: Record<Tone, string> = {
  success: 'text-success',
  warning: 'text-warning',
  danger: 'text-danger',
  info: 'text-info',
  accent: 'text-accent',
  purple: 'text-purple',
  pink: 'text-pink',
  neutral: 'text-neutral',
}

/** A faint fill and edge of the tone, behind text of the same tone or of the default colour. */
export const toneTint: Record<Tone, string> = {
  success: 'border-success/30 bg-success/10',
  warning: 'border-warning/30 bg-warning/10',
  danger: 'border-danger/30 bg-danger/10',
  info: 'border-info/30 bg-info/10',
  accent: 'border-accent/30 bg-accent/10',
  purple: 'border-purple/30 bg-purple/10',
  pink: 'border-pink/30 bg-pink/10',
  neutral: 'border-neutral/30 bg-neutral/10',
}
