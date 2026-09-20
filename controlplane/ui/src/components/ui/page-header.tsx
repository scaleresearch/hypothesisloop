import type { ReactNode } from 'react'
import { Button } from '@/components/ui/button'

interface PageHeaderProps {
  title: ReactNode
  description?: ReactNode
  actions?: ReactNode
  /** Where this page was reached from, shown over the title. */
  back?: { label: ReactNode; onClick: () => void }
}

/** The title at the top of every page, with an optional right-aligned action row. */
export function PageHeader({ title, description, actions, back }: PageHeaderProps) {
  return (
    <div className="mb-6 flex items-end justify-between gap-4 pb-1.5">
      <div className="min-w-0">
        {back && <Button variant="ghost" size="sm" className="-ml-2.5 mb-1 font-normal" onClick={back.onClick}>← {back.label}</Button>}
        <h1 className="flex flex-wrap items-center gap-2.5 break-words text-2xl font-bold tracking-tight">{title}</h1>
        {description && <div className="mt-1 text-sm text-muted">{description}</div>}
      </div>
      {actions && <div className="mb-0.5 flex flex-none items-center gap-2">{actions}</div>}
    </div>
  )
}
