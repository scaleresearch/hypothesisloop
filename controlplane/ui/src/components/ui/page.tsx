import type { ReactNode } from 'react'
import { clsx } from 'clsx'
import { Button } from '@/components/ui/button'
import { Heading } from '@/components/ui/text'

interface PageProps {
  title: ReactNode
  description?: ReactNode
  actions?: ReactNode
  /** Where this page was reached from, shown over the title. */
  back?: { label: ReactNode; onClick: () => void }
  /** The body fills the window's height, for a canvas that grows into it. */
  fill?: boolean
  children?: ReactNode
}

/** A page: its title with an optional right-aligned action row, then its sections, evenly spaced. */
export function Page({ title, description, actions, back, fill, children }: PageProps) {
  return (
    <div className={clsx('flex flex-col gap-5', fill && 'min-h-workspace')}>
      <header className="mb-3 flex flex-wrap items-end justify-between gap-x-6 gap-y-4">
        <div className="min-w-0">
          {back && <Button variant="ghost" size="sm" className="-ml-3 mb-2 font-normal" onClick={back.onClick}>← {back.label}</Button>}
          <Heading level={1} className="flex flex-wrap items-center gap-3 break-words">{title}</Heading>
          {description && <div className="mt-2 text-sm text-muted">{description}</div>}
        </div>
        {actions && <div className="flex flex-none items-center gap-2">{actions}</div>}
      </header>
      {children}
    </div>
  )
}
