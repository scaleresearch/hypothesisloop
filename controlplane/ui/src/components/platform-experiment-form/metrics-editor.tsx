import { COMMON_ML_METRICS } from '@/types'
import type { MetricDefinition } from '@/types'
import { Button } from '@/components/ui/button'
import { Combobox } from '@/components/ui/combobox'
import type { ComboboxChoice } from '@/components/ui/combobox'
import { Inline, Stack } from '@/components/ui/stack'
import { Pill } from '@/components/ui/text'

const arrow = (direction: MetricDefinition['direction']) => direction === 'maximize' ? '↑ max' : '↓ min'

/** The metrics to add for what is typed: a key of one's own first, which maximizes until flipped, then the common ones. */
function choices(taken: string[], typed: string): ComboboxChoice<MetricDefinition>[] {
  const custom = typed !== '' && !taken.includes(typed) && !COMMON_ML_METRICS.some(m => m.key === typed)
  const common = COMMON_ML_METRICS.filter(m => m.key !== 'custom' && !taken.includes(m.key)
    && (typed === '' || `${m.key} ${m.label}`.toLowerCase().includes(typed.toLowerCase())))
  return [
    ...(custom ? [{ key: `custom:${typed}`, value: { key: typed, direction: 'maximize' as const }, label: `Add ${typed}`, badge: arrow('maximize') }] : []),
    ...common.map(m => ({ key: m.key, value: { key: m.key, direction: m.direction }, label: m.label, badge: arrow(m.direction) })),
  ]
}

/** The metrics of an experiment: the first ranks the leaderboard, each has a direction. */
export function MetricsEditor({ metrics, onChange }: { metrics: MetricDefinition[]; onChange: (m: MetricDefinition[]) => void }) {
  function toggleDir(key: string) {
    onChange(metrics.map(m => m.key === key ? { ...m, direction: m.direction === 'maximize' ? 'minimize' : 'maximize' } : m))
  }

  function makePrimary(key: string) {
    const picked = metrics.find(m => m.key === key)
    if (picked) onChange([picked, ...metrics.filter(m => m.key !== key)])
  }

  return (
    <Stack>
      <Combobox
        aria-label="Add a metric"
        placeholder="Add a metric: pick one or type its key"
        choices={typed => choices(metrics.map(m => m.key), typed)}
        onPick={m => onChange([...metrics, m])}
      />
      {metrics.length > 0 && (
        <Inline gap="xs">
          {metrics.map((m, i) => (
            <Pill key={m.key} tone={m.direction === 'maximize' ? 'success' : 'warning'} active={i === 0}>
              {i === 0
                ? <span title="Primary: ranks the leaderboard">★</span>
                : <Button type="button" variant="ghost" size="icon" onClick={() => makePrimary(m.key)} title="Make primary">☆</Button>}
              <Button type="button" variant="ghost" size="icon" onClick={() => toggleDir(m.key)} title="Flip between maximize and minimize">
                {m.direction === 'maximize' ? '↑' : '↓'}
              </Button>
              {m.key}
              <Button type="button" variant="ghost" size="icon" onClick={() => onChange(metrics.filter(x => x.key !== m.key))} title="Remove this metric">×</Button>
            </Pill>
          ))}
        </Inline>
      )}
    </Stack>
  )
}
