import type { HTMLAttributes } from 'react'
import { clsx } from 'clsx'
import type { Tone } from '@/lib/colors'
import { toneText, toneTint } from '@/components/ui/tone'

/** The caption over a value or a group of controls, the same as the label of a FormField. */
export const captionClass = 'text-xs font-medium'

export function Eyebrow({ tone, className, ...props }: HTMLAttributes<HTMLSpanElement> & { tone?: Tone }) {
  return <span className={clsx(captionClass, tone ? toneText[tone] : 'text-muted', className)} {...props} />
}

export function Kbd({ className, ...props }: HTMLAttributes<HTMLElement>) {
  return <kbd className={clsx('flex-none rounded-sm border border-border-strong bg-surface-raised px-1.5 py-px font-mono text-2xs text-muted', className)} {...props} />
}

/** A small rounded label: a metric, a stage, a policy. `tone` tints it, `active` rings the chosen one. */
export function Pill({ tone, active, className, ...props }: HTMLAttributes<HTMLSpanElement> & { tone?: Tone; active?: boolean }) {
  return (
    <span
      className={clsx(
        'inline-flex items-center gap-1 rounded-full border px-2.5 py-0.5 text-xs',
        tone ? [toneText[tone], toneTint[tone]] : 'bg-surface-inset text-muted',
        active && 'border-accent ring-1 ring-inset ring-accent',
        className,
      )}
      {...props}
    />
  )
}
