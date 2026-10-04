import type { ReactNode } from 'react'
import { clsx } from 'clsx'
import type { Tone } from '@/lib/colors'
import { Dot } from '@/components/ui/dot'
import { toneTint } from '@/components/ui/tone'

/** Skeleton bars for a block whose data is in flight. */
export function Loading({ text = 'Loading…', rows = 3 }: { text?: string; rows?: number }) {
  return (
    <div className="flex flex-col gap-2 py-0.5" role="status" aria-label={text}>
      {Array.from({ length: rows }).map((_, i) => (
        <div key={i} className={clsx('h-3 animate-pulse rounded-sm bg-track', i === rows - 1 ? 'w-3/5' : 'w-full')} />
      ))}
    </div>
  )
}

/** The mark of an action in flight, sized by the text around it. */
export function Spinner({ className }: { className?: string }) {
  return <span aria-hidden="true" className={clsx('inline-block size-em flex-none animate-spin rounded-full border-2 border-current border-r-transparent', className)} />
}

// error-banner and text-error carry no style: the browser suite finds errors by them.
export function ErrorMessage({ children, className }: { children: ReactNode; className?: string }) {
  return (
    <div role="alert" className={clsx('error-banner flex items-center gap-2 rounded border border-danger-edge bg-danger-tint px-3.5 py-2.5 text-sm text-danger', className)}>
      <Dot tone="danger" size="sm" />
      <span>{children}</span>
    </div>
  )
}

/** A tinted note in the flow of a page: why a job stopped, what the scheduler is waiting for. */
export function Callout({ tone, className, children }: { tone: Tone; className?: string; children: ReactNode }) {
  return <div className={clsx('rounded border px-3.5 py-2.5 text-sm', toneTint[tone], className)}>{children}</div>
}

/** An error beside the control it is about. */
export function InlineError({ children, className }: { children: ReactNode; className?: string }) {
  return <span role="alert" className={clsx('text-error text-xs text-danger', className)}>{children}</span>
}
