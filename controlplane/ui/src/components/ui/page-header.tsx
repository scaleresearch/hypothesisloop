import type { ReactNode } from 'react'

interface PageHeaderProps {
  title: string
  description?: ReactNode
  actions?: ReactNode
}

/** The `.wa-title` bar at the top of every page, with an optional right-aligned action row. */
export function PageHeader({ title, description, actions }: PageHeaderProps) {
  return (
    <div className="wa-title" style={{ display: 'flex', alignItems: 'flex-end', justifyContent: 'space-between' }}>
      <div>
        <h1>{title}</h1>
        {description && <p>{description}</p>}
      </div>
      {actions && <div style={{ display: 'flex', gap: 8, marginBottom: 2 }}>{actions}</div>}
    </div>
  )
}
