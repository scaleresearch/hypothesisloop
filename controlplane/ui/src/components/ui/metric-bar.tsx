import { semantic } from '@/lib/colors'

interface MetricBarProps {
  value: number
  max: number
  /** Optional override; by default green/amber/red based on percentage used. */
  color?: string
}

/** Horizontal progress bar used for budget/quota utilisation. */
export function MetricBar({ value, max, color }: MetricBarProps) {
  const pct = max > 0 ? Math.min(100, Math.round((value / max) * 100)) : 0
  const fillColor = color ?? (pct > 90 ? semantic.danger : pct > 70 ? semantic.warning : semantic.success)
  return (
    <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
      <div className="metric-bar-track" style={{ flex: 1 }}>
        <div className="metric-bar-fill" style={{ width: `${pct}%`, background: fillColor, boxShadow: `0 0 8px ${fillColor}66` }} />
      </div>
      <span className="mono text-muted" style={{ width: 36, textAlign: 'right', fontSize: 11 }}>{pct}%</span>
    </div>
  )
}
