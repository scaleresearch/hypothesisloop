import type { HTMLAttributes } from 'react'
import { clsx } from 'clsx'
import type { Tone } from '@/lib/colors'
import { toneText, toneTint } from '@/components/ui/tone'

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
        tone ? [toneText[tone], toneTint[tone]] : 'bg-surface-inset text-muted',
        active && 'border-accent ring-1 ring-inset ring-accent',
        className,
      )}
      {...props}
    />
  )
}
