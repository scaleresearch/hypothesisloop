import type { ReactNode } from 'react'
import Link from 'next/link'

interface StatTileProps {
  label: string
  value: ReactNode
  sub?: string
  color?: string
  /** When set, the whole tile becomes a link (e.g. to a pre-filtered job list). */
  href?: string
}

/** KPI box used in stat/summary rows (dashboard, jobs, platform-experiment detail). */
export function StatTile({ label, value, sub, color, href }: StatTileProps) {
  const content = (
    <>
      <div className="uppercase-label" style={{ marginBottom: 4 }}>{label}</div>
      <div className="mono stat-tile-value" style={color ? { color } : undefined} title={typeof value === 'string' ? value : undefined}>{value}</div>
      {sub && <div className="stat-tile-sub" title={sub}>{sub}</div>}
    </>
  )
  if (href) {
    return (
      <Link href={href} className="stat-tile" style={{ display: 'block', textDecoration: 'none', color: 'inherit' }}>
        {content}
      </Link>
    )
  }
  return <div className="stat-tile">{content}</div>
}
