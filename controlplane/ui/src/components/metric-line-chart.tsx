'use client'

import { CartesianGrid, Legend, Line, LineChart, ResponsiveContainer, Tooltip, XAxis, YAxis } from 'recharts'
import { EmptyState } from '@/components/ui/empty-state'
import { chart, semantic, seriesPalette } from '@/lib/colors'
import { formatMetric } from '@/lib/format'
import type { MetricDataPoint } from '@/types/api'

// Bounded scores (roughly 0-1) get their own axis so unbounded metrics in the tens and more
// do not flatten them to a hairline.
const BOUNDED_METRIC_RE = /auroc|accuracy|precision|recall|f1|auc/i

/**
 * Every metric of the points as a line over training progress, which reads where a time axis of a
 * job of a few seconds does not. `primary` is drawn first and in the primary color. Fills its parent's height.
 */
export function MetricLineChart({ points, primary }: { points: MetricDataPoint[]; primary?: string }) {
  if (points.length === 0) return <EmptyState>No data yet</EmptyState>
  // The API answers one block per series, and a retried job has two of the same metric: ordered
  // by when they were recorded, a line never runs back on itself.
  const rows = [...points]
    .sort((a, b) => new Date(a.recorded_at as string).getTime() - new Date(b.recorded_at as string).getTime())
    .map(p => ({ frac: Math.round(p.fraction_complete * 1000) / 10, [p.metric_name]: p.metric_value }))
  const names = Array.from(new Set(points.map(p => p.metric_name))).sort((a, b) =>
    a === primary ? -1 : b === primary ? 1 : a.localeCompare(b))
  const bounded = (name: string) => BOUNDED_METRIC_RE.test(name)
  const twoAxes = names.some(bounded) && !names.every(bounded)
  const axisOf = (name: string) => (twoAxes && !bounded(name) ? 'right' : 'left')
  return (
    <ResponsiveContainer width="100%" height="100%">
      <LineChart data={rows} margin={{ top: 4, right: 8, left: 0, bottom: 0 }}>
        <CartesianGrid strokeDasharray="3 3" stroke={chart.grid} />
        <XAxis dataKey="frac" type="number" domain={[0, 100]} tickFormatter={v => `${v}%`} tick={chart.tick} />
        <YAxis yAxisId="left" domain={['auto', 'auto']} tick={chart.tick} width={48} />
        {twoAxes && <YAxis yAxisId="right" orientation="right" domain={['auto', 'auto']} tick={chart.tick} width={48} />}
        <Tooltip formatter={(v: number) => formatMetric(v)} labelFormatter={l => `${l}% of training`} contentStyle={chart.tooltip.contentStyle} labelStyle={chart.tooltip.labelStyle} />
        <Legend wrapperStyle={chart.legend} />
        {names.map((name, i) => (
          <Line
            key={name}
            yAxisId={axisOf(name)}
            dataKey={name}
            name={twoAxes ? `${name} (${axisOf(name)} axis)` : name}
            stroke={name === primary ? semantic.primary : seriesPalette[i % seriesPalette.length]}
            dot={{ r: 2 }}
            strokeWidth={1.5}
            type="monotone"
            connectNulls
            isAnimationActive={false}
          />
        ))}
      </LineChart>
    </ResponsiveContainer>
  )
}
