import { clsx } from 'clsx'

/** Horizontal bar of how much of a budget or quota is used: green, amber past 70%, red past 90%. */
export function MetricBar({ value, max }: { value: number; max: number }) {
  const pct = max > 0 ? Math.min(100, Math.round((value / max) * 100)) : 0
  return (
    <div className="flex items-center gap-2">
      <div className="h-1.5 flex-1 overflow-hidden rounded-full bg-track" role="progressbar" aria-valuenow={pct} aria-valuemin={0} aria-valuemax={100}>
        <div
          className={clsx('h-full rounded-full transition-[width] duration-300', pct > 90 ? 'bg-danger' : pct > 70 ? 'bg-warning' : 'bg-success')}
          style={{ width: `${pct}%` }}
        />
      </div>
      <span className="w-9 text-right font-mono text-xs text-muted">{pct}%</span>
    </div>
  )
}
