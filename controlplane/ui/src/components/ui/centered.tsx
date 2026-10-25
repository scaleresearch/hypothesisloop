import type { ReactNode } from 'react'
import { clsx } from 'clsx'
import { Heading } from '@/components/ui/text'

/**
 * One narrow column in the middle of an empty window: signing in, accepting an invite, an account
 * that cannot be loaded. `head`, usually the logo, sits in the top left corner where the nav keeps it.
 */
export function Centered({ head, title, wide, children }: { head?: ReactNode; title?: ReactNode; wide?: boolean; children: ReactNode }) {
  return (
    <div className="w-full">
      {head && <header className="px-5 pt-6">{head}</header>}
      <main className={clsx('mx-auto mt-drop grid w-full content-start gap-6 px-5', wide ? 'max-w-xl' : 'max-w-sm')}>
        {title && <Heading level={1}>{title}</Heading>}
        {children}
      </main>
    </div>
  )
}
