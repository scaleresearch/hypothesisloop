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
    <div className="mb-8 flex flex-wrap items-end justify-between gap-x-6 gap-y-4">
      <div className="min-w-0">
        {back && <Button variant="ghost" size="sm" className="-ml-3 mb-2 font-normal" onClick={back.onClick}>← {back.label}</Button>}
        <h1 className="flex flex-wrap items-center gap-3 break-words text-2xl font-normal text-foreground">{title}</h1>
        {description && <div className="mt-2 text-sm text-muted">{description}</div>}
      </div>
      {actions && <div className="flex flex-none items-center gap-2">{actions}</div>}
    </div>
  )
}
