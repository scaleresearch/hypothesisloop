import type { ComponentProps, HTMLAttributes, ReactNode } from 'react'
import { clsx } from 'clsx'
import { Link } from '@/components/ui/link'

type DivProps = HTMLAttributes<HTMLDivElement>

/** The panel every page section sits in. */
export function Card({ className, ...props }: DivProps) {
  return (
    <div
      className={clsx(
        'mb-5 animate-rise overflow-hidden rounded-lg border bg-surface',
        className,
      )}
      {...props}
    />
  )
}

export function CardHeader({ className, ...props }: DivProps) {
  return (
    <div
      className={clsx('flex items-center gap-2 border-b px-5 py-3.5 text-sm font-medium text-foreground', className)}
      {...props}
    />
  )
}

/** The heading of a card, inside CardHeader. `note` is what stands beside it: a count, a scope. */
export function CardTitle({ note, children }: { note?: ReactNode; children: ReactNode }) {
  return (
    <h2 className="min-w-0 text-base font-medium text-foreground">
      {children}
      {note !== undefined && note !== null && note !== false && <span className="ml-2 text-xs font-normal text-muted">{note}</span>}
    </h2>
  )
}

export function CardContent({ className, scrollX, ...props }: DivProps & { scrollX?: boolean }) {
  return <div className={clsx('p-5', scrollX && 'overflow-x-auto', className)} {...props} />
}

/** A tile inside a card: a per-job chart, an agent's job list. */
export function InsetCard({ className, ...props }: DivProps) {
  return <div className={clsx('rounded border bg-surface-inset p-3', className)} {...props} />
}

/** A card that is one link to another page. */
export function CardLink({ className, ...props }: ComponentProps<typeof Link>) {
  return (
    <Link
      className={clsx('block animate-rise rounded-lg border bg-surface p-5 transition hover:border-border-strong hover:shadow-md', className)}
      {...props}
    />
  )
}

/** A row of a list inside a card that leads to the record it names. */
export function RowLink({ className, ...props }: ComponentProps<typeof Link>) {
  return (
    <Link
      className={clsx('flex w-full min-w-0 items-center gap-2 rounded px-2.5 py-1.5 text-xs text-foreground transition-colors hover:bg-hover', className)}
      {...props}
    />
  )
}
