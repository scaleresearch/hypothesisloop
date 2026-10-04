import type { ReactNode } from 'react'
import { clsx } from 'clsx'

/** What stands where a list or a chart has nothing to show. `framed` draws the outline of the missing content. */
export function EmptyState({ children, action, framed }: { children: ReactNode; action?: ReactNode; framed?: boolean }) {
  return (
    <div className={clsx('grid justify-items-center gap-3 p-8 text-center text-sm text-subtle', framed && 'rounded-lg border border-dashed')}>
      <p>{children}</p>
      {action}
    </div>
  )
}
