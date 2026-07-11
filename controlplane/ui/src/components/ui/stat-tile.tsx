import type { ReactNode } from 'react'

interface StatTileProps {
  label: string
  value: ReactNode
  sub?: string
  color?: string
}

/** KPI box used in stat/summary rows (dashboard, jobs, platform-experiment detail). */
export function StatTile({ label, value, sub, color }: StatTileProps) {
  return (
    <div className="stat-tile">
      <div className="uppercase-label" style={{ marginBottom: 4 }}>{label}</div>
      <div className="mono stat-tile-value" style={color ? { color } : undefined}>{value}</div>
      {sub && <div className="stat-tile-sub">{sub}</div>}
    </div>
  )
}
