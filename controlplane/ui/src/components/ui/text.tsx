import type { HTMLAttributes } from 'react'
import { clsx } from 'clsx'

/** The small uppercase caption over a value or a group of controls. */
export function Eyebrow({ className, ...props }: HTMLAttributes<HTMLSpanElement>) {
  return <span className={clsx('text-2xs font-semibold uppercase tracking-wider text-muted', className)} {...props} />
}

export function Kbd({ className, ...props }: HTMLAttributes<HTMLElement>) {
  return <kbd className={clsx('flex-none rounded-sm border border-border-strong bg-surface-raised px-1.5 py-px font-mono text-2xs text-muted', className)} {...props} />
}
