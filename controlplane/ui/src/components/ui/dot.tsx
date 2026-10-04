import { clsx } from 'clsx'
import type { Tone } from '@/lib/colors'

const toneFill: Record<Tone, string> = {
  success: 'bg-success',
  warning: 'bg-warning',
  danger: 'bg-danger',
  info: 'bg-info',
  accent: 'bg-accent',
  neutral: 'bg-neutral',
}

/**
 * A round mark: in a tone, in a colour of lib/colors (a chart series), or in the colour of the
 * text around it when given neither. `sm` sits inside a line of small text, `md` beside a name.
 */
export function Dot({ tone, color, size = 'md', pulse, className }: {
  tone?: Tone
  color?: string
  size?: 'sm' | 'md'
  pulse?: boolean
  className?: string
}) {
  return (
    <span
      aria-hidden="true"
      className={clsx(
        'inline-block flex-none rounded-full',
        size === 'sm' ? 'size-1.5' : 'size-2',
        tone ? toneFill[tone] : !color && 'bg-current',
        pulse && 'animate-pulse',
        className,
      )}
      style={color ? { background: color } : undefined}
    />
  )
}
