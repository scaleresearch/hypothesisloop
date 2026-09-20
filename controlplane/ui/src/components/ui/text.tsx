import type { HTMLAttributes } from 'react'
import { clsx } from 'clsx'
import type { Tone } from '@/lib/colors'
import { toneText } from '@/components/ui/tone'

/** The small uppercase caption over a value or a group of controls. */
export function Eyebrow({ tone, className, ...props }: HTMLAttributes<HTMLSpanElement> & { tone?: Tone }) {
  return <span className={clsx('text-2xs font-semibold uppercase tracking-wider', tone ? toneText[tone] : 'text-muted', className)} {...props} />
}

export function Kbd({ className, ...props }: HTMLAttributes<HTMLElement>) {
  return <kbd className={clsx('flex-none rounded-sm border border-border-strong bg-surface-raised px-1.5 py-px font-mono text-2xs text-muted', className)} {...props} />
}

/** A small rounded label: a metric, a stage, a policy. `tone` tints it, `active` rings the chosen one. */
export function Pill({ tone, active, className, ...props }: HTMLAttributes<HTMLSpanElement> & { tone?: Tone; active?: boolean }) {
  return (
    <span
      className={clsx(
        'inline-flex items-center gap-1 rounded-full border px-2 py-0.5 font-mono text-2xs',
        tone ? [toneText[tone], pillTint[tone]] : 'bg-surface-inset text-muted',
        active && 'border-accent ring-1 ring-inset ring-accent',
        className,
      )}
      {...props}
    />
  )
}

const pillTint: Record<Tone, string> = {
  success: 'border-success/30 bg-success/10',
  warning: 'border-warning/30 bg-warning/10',
  danger: 'border-danger/30 bg-danger/10',
  info: 'border-info/30 bg-info/10',
  accent: 'border-accent/30 bg-accent/10',
  purple: 'border-purple/30 bg-purple/10',
  pink: 'border-pink/30 bg-pink/10',
  neutral: 'border-neutral/30 bg-neutral/10',
}
