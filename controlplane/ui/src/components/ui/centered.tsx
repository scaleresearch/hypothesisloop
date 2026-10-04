import type { ReactNode } from 'react'
import { clsx } from 'clsx'
import { Heading } from '@/components/ui/text'

/**
 * One narrow column in the middle of an empty window: signing in, accepting an invite, an account
 * that cannot be loaded. `head` stands over the title, usually the logo.
 */
export function Centered({ head, title, wide, children }: { head?: ReactNode; title?: ReactNode; wide?: boolean; children: ReactNode }) {
  return (
    <main className={clsx('mx-auto mt-drop grid w-full content-start gap-4 px-5', wide ? 'max-w-xl' : 'max-w-sm')}>
      {head}
      {title && <Heading level={1}>{title}</Heading>}
      {children}
    </main>
  )
}
