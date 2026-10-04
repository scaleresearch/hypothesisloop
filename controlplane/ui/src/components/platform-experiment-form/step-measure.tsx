import { FieldGroup, FormField } from '@/components/ui/form-field'
import { InfoTip } from '@/components/ui/info-tip'
import { Input } from '@/components/ui/input'
import { Inline } from '@/components/ui/stack'
import { Pill } from '@/components/ui/text'
import { MetricsEditor } from '@/components/platform-experiment-form/metrics-editor'
import { LOCKED } from '@/components/platform-experiment-form/model'
import type { StepProps } from '@/components/platform-experiment-form/model'

/** Step 2: the metrics and how long it runs. */
export function StepMeasure({ form, set, locked }: StepProps) {
  return (
    <>
      <FieldGroup
        label="Metrics"
        hint={locked ? LOCKED : <>★ ranks the leaderboard; ↑↓ flips a direction.<InfoTip>At a stage cut an agent survives if it survives on at least one metric, so a specialist is not cut for a metric it never targeted.</InfoTip></>}
      >
        {locked ? (
          <Inline gap="xs">
            {form.metrics.map(m => <Pill key={m.key}>{m.key} {m.direction === 'maximize' ? '↑' : '↓'}</Pill>)}
          </Inline>
        ) : (
          <MetricsEditor metrics={form.metrics} onChange={m => set('metrics', m)} />
        )}
      </FieldGroup>
      <FormField label="Duration (hours)" hint={locked ? LOCKED : 'Counted from the start.'}>
        <Input type="number" className="w-40" min={0} step="any" value={form.duration_hours} disabled={locked} onChange={e => set('duration_hours', e.target.value)} />
      </FormField>
    </>
  )
}
