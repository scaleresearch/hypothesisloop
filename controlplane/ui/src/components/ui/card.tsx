import type { HTMLAttributes } from 'react'
import { clsx } from 'clsx'

type DivProps = HTMLAttributes<HTMLDivElement>

/** The panel every page section sits in. */
export function Card({ className, ...props }: DivProps) {
  return (
    <div
      className={clsx(
        'mb-4 animate-rise overflow-hidden rounded-lg border bg-surface shadow-sm transition-shadow hover:border-border-strong hover:shadow-md',
        className,
      )}
      {...props}
    />
  )
}

export function CardHeader({ className, ...props }: DivProps) {
  return (
    <div
      className={clsx('flex items-center gap-2 border-b bg-surface-inset px-4 py-3 text-sm font-semibold text-foreground', className)}
      {...props}
    />
  )
}

export function CardContent({ className, scrollX, ...props }: DivProps & { scrollX?: boolean }) {
  return <div className={clsx('p-4', scrollX && 'overflow-x-auto', className)} {...props} />
}

/** A tile inside a card: a per-job chart, an agent's job list. */
export function InsetCard({ className, ...props }: DivProps) {
  return <div className={clsx('rounded border bg-surface-inset p-2.5 transition-colors hover:border-border-strong', className)} {...props} />
}
