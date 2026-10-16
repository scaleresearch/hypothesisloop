import { useState } from 'react'
import { COMMON_ML_METRICS } from '@/components/platform-experiment-form/common-metrics'
import type { MetricDefinition } from '@/types/platform-experiments'
import { Button } from '@/components/ui/button'
import { Combobox } from '@/components/ui/combobox'
import type { ComboboxChoice } from '@/components/ui/combobox'
import { Input } from '@/components/ui/input'
import { Select } from '@/components/ui/select'
import { Inline, Stack } from '@/components/ui/stack'
import { Code, Text } from '@/components/ui/text'

const arrow = (direction: MetricDefinition['direction']) => direction === 'maximize' ? '↑ max' : '↓ min'

type Role = NonNullable<MetricDefinition['role']>

const ROLES: { value: Role; label: string }[] = [
  { value: 'ranking', label: 'Ranked' },
  { value: 'constraint', label: 'Gate' },
  { value: 'attribute', label: 'Tracked' },
]

const roleOf = (m: MetricDefinition): Role => m.role ?? 'ranking'

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

/**
 * A number field held as typed, so that a half-typed value such as "-" or "0." stays while it is
 * typed. It hands on a number, or undefined while it is empty or no number.
 */
function NumberField({ value, onChange, ...props }: {
  value: number | undefined
  onChange: (value: number | undefined) => void
  'aria-label': string
  placeholder?: string
}) {
  const [typed, setTyped] = useState(value === undefined ? '' : String(value))
  return (
    <Input
      fieldSize="sm"
      inputMode="decimal"
      className="w-24"
      value={typed}
      onChange={e => {
        setTyped(e.target.value)
        const n = Number(e.target.value)
        onChange(e.target.value.trim() === '' || !Number.isFinite(n) ? undefined : n)
      }}
      {...props}
    />
  )
}

// target_vs_baseline is signed in the metric's direction; the person types how far past the control, in percent.
const targetPct = (m: MetricDefinition) => m.target_vs_baseline === undefined ? undefined : Math.round(Math.abs(m.target_vs_baseline) * 1000) / 10
const signedTarget = (pct: number, direction: MetricDefinition['direction']) => (direction === 'maximize' ? pct : -pct) / 100

/**
 * The metrics of an experiment, each with a direction and a role: ranked, a gate every job must
 * pass, or tracked only. The first ranked one ranks the leaderboard and may carry the target.
 */
export function MetricsEditor({ metrics, onChange }: { metrics: MetricDefinition[]; onChange: (m: MetricDefinition[]) => void }) {
  const primary = metrics.find(m => roleOf(m) === 'ranking')?.key

  function change(key: string, to: (m: MetricDefinition) => MetricDefinition) {
    onChange(metrics.map(m => m.key === key ? to(m) : m))
  }

  function flip(m: MetricDefinition): MetricDefinition {
    const direction = m.direction === 'maximize' ? 'minimize' : 'maximize'
    return { ...m, direction, ...(m.target_vs_baseline !== undefined ? { target_vs_baseline: -m.target_vs_baseline } : {}) }
  }

  // A metric keeps only what its role reads: a bound on a gate, a target on a ranked one.
  function withRole(m: MetricDefinition, role: Role): MetricDefinition {
    const { bound, target_vs_baseline, ...rest } = m
    if (role === 'constraint') return { ...rest, role, ...(bound !== undefined ? { bound } : {}) }
    if (role === 'ranking') return { ...rest, role, ...(target_vs_baseline !== undefined ? { target_vs_baseline } : {}) }
    return { ...rest, role }
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
        <Stack as="ul" gap="xs">
          {metrics.map(m => {
            const role = roleOf(m)
            return (
              <Inline as="li" key={m.key} gap="sm" nowrap>
                {m.key === primary
                  ? <span className="w-5 text-center" title="Primary: ranks the leaderboard">★</span>
                  : <Button type="button" variant="ghost" size="icon" className="size-6" disabled={role !== 'ranking'} onClick={() => makePrimary(m.key)} aria-label={`Make ${m.key} the primary metric`} title="Make primary: it ranks the leaderboard">☆</Button>}
                <Code className="min-w-0 flex-1 truncate">{m.key}</Code>
                <Button type="button" size="sm" onClick={() => change(m.key, flip)} aria-label={`${m.key}: ${m.direction === 'maximize' ? 'higher' : 'lower'} is better. Flip`} title="Flip between maximize and minimize">
                  {arrow(m.direction)}
                </Button>
                {role === 'reference'
                  ? <Text as="span" variant="hint" className="w-28">Reference</Text>
                  : <Select fieldSize="sm" className="w-28" aria-label={`Role of ${m.key}`} value={role} options={ROLES} onChange={r => change(m.key, x => withRole(x, r))} />}
                {role === 'constraint' && (
                  <Inline gap="xs" nowrap>
                    <Text as="span" variant="hint">{m.direction === 'maximize' ? 'at least' : 'at most'}</Text>
                    <NumberField aria-label={`Bound of ${m.key}`} placeholder="bound" value={m.bound} onChange={bound => change(m.key, x => ({ ...withRole(x, 'constraint'), ...(bound !== undefined ? { bound } : {}) }))} />
                  </Inline>
                )}
                {m.key === primary && (
                  <Inline gap="xs" nowrap>
                    <Text as="span" variant="hint">target</Text>
                    <NumberField
                      aria-label={`Target of ${m.key}, percent past the baseline`}
                      placeholder="none"
                      value={targetPct(m)}
                      onChange={pct => change(m.key, x => ({ ...withRole(x, 'ranking'), ...(pct !== undefined ? { target_vs_baseline: signedTarget(pct, x.direction) } : {}) }))}
                    />
                    <Text as="span" variant="hint">% past baseline</Text>
                  </Inline>
                )}
                <Button type="button" variant="ghost" size="icon" className="size-6" onClick={() => onChange(metrics.filter(x => x.key !== m.key))} aria-label={`Remove ${m.key}`} title="Remove this metric">×</Button>
              </Inline>
            )
          })}
        </Stack>
      )}
    </Stack>
  )
}
