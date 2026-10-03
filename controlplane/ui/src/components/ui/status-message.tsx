import type { ReactNode } from 'react'
import { clsx } from 'clsx'
import type { Tone } from '@/lib/colors'
import { toneTint } from '@/components/ui/tone'

/** Skeleton bars for a block whose data is in flight. */
export function Loading({ text = 'Loading…', rows = 3 }: { text?: string; rows?: number }) {
  return (
    <div className="flex flex-col gap-2 py-0.5" role="status" aria-label={text}>
      {Array.from({ length: rows }).map((_, i) => (
        <Skeleton key={i} className={i === rows - 1 ? 'w-3/5' : 'w-full'} />
      ))}
    </div>
  )
}

export function Skeleton({ className }: { className?: string }) {
  return (
    <div
      className={clsx(
        'h-3 animate-pulse rounded-sm bg-track',
        className,
      )}
    />
  )
}

/** The mark of an action in flight, sized by the text around it. */
export function Spinner({ className }: { className?: string }) {
  return <span aria-hidden="true" className={clsx('inline-block h-[1em] w-[1em] flex-none animate-spin rounded-full border-2 border-current border-r-transparent', className)} />
}

// error-banner and text-error carry no style: the browser suite finds errors by them.
export function ErrorMessage({ children, className }: { children: ReactNode; className?: string }) {
  return (
    <div role="alert" className={clsx('error-banner flex items-center gap-2 rounded border border-danger-edge bg-danger-tint px-3.5 py-2.5 text-sm text-danger', className)}>
      <span aria-hidden="true" className="h-1.5 w-1.5 flex-none rounded-full bg-danger" />
      <span>{children}</span>
    </div>
  )
}

/** A tinted note in the flow of a page: why a job stopped, what the scheduler is waiting for. */
export function Callout({ tone, className, children }: { tone: Tone; className?: string; children: ReactNode }) {
  return <div className={clsx('mb-3.5 rounded border px-3.5 py-2.5 text-sm', toneTint[tone], className)}>{children}</div>
}

/** An error beside the control it is about. */
export function InlineError({ children, className }: { children: ReactNode; className?: string }) {
  return <span role="alert" className={clsx('text-error text-xs text-danger', className)}>{children}</span>
}

export function EmptyState({ children, action }: { children: ReactNode; action?: ReactNode }) {
  return (
    <div className="grid justify-items-center gap-3 p-8 text-center text-sm text-subtle">
      <p>{children}</p>
      {action}
    </div>
  )
}
