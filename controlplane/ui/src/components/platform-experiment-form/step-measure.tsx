import { FieldGroup, FormField } from '@/components/ui/form-field'
import { InfoTip } from '@/components/ui/info-tip'
import { Input } from '@/components/ui/input'
import { Inline } from '@/components/ui/stack'
import { Pill } from '@/components/ui/text'
import { MetricsEditor } from '@/components/platform-experiment-form/metrics-editor'
import { LOCKED } from '@/components/platform-experiment-form/model'
import type { StepProps } from '@/components/platform-experiment-form/model'

const ROLE_WORD = { constraint: 'gate', attribute: 'tracked', reference: 'reference' } as const

/** Step 2: the metrics and how long it runs. */
export function StepMeasure({ form, set, isEdit, locked }: StepProps) {
  return (
    <>
      <FieldGroup
        label="Metrics"
        hint={locked('metrics') ? LOCKED : <>★ ranks the leaderboard and its target is the win agents aim for.<InfoTip>A job that misses a gate&apos;s bound ranks nowhere, and a tracked metric is only shown. The target is how far past the measured baseline counts as a win, in percent. At a stage cut an agent survives if it survives on at least one ranked metric, so a specialist is not cut for a metric it never targeted.</InfoTip></>}
      >
        {locked('metrics') ? (
          <Inline gap="xs">
            {form.metrics.map(m => <Pill key={m.key}>{m.key} {m.direction === 'maximize' ? '↑' : '↓'}{m.role && m.role !== 'ranking' ? ` · ${ROLE_WORD[m.role]}` : ''}</Pill>)}
          </Inline>
        ) : (
          <MetricsEditor metrics={form.metrics} onChange={m => set('metrics', m)} />
        )}
      </FieldGroup>
      <FormField label="Duration (hours)" hint={isEdit ? 'Research time, counted from the start of the run. The agents are told when it changes.' : 'Research time, counted from the start of the run.'}>
        <Input type="number" className="w-40" min={0} step="any" value={form.duration_hours} disabled={locked('duration_hours')} onChange={e => set('duration_hours', e.target.value)} />
      </FormField>
    </>
  )
}
