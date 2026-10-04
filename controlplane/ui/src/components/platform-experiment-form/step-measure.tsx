import { FieldGroup, FormField } from '@/components/ui/form-field'
import { InfoTip } from '@/components/ui/info-tip'
import { Input } from '@/components/ui/input'
import { Segmented } from '@/components/ui/segmented'
import { Inline, Stack } from '@/components/ui/stack'
import { Pill } from '@/components/ui/text'
import { MetricsEditor } from '@/components/platform-experiment-form/metrics-editor'
import { LOCKED, ranked } from '@/components/platform-experiment-form/model'
import type { StepProps } from '@/components/platform-experiment-form/model'

/** Step 2: the metrics, the baseline researchers are ranked against, and how long it runs. */
export function StepMeasure({ form, set, isEdit, locked }: StepProps) {
  const baseline = form.baseline
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
      {!isEdit && (
        <FieldGroup
          label="Baseline"
          hint={<>What researchers are ranked against.<InfoTip>An agent measures it on the repository before the run starts. Given here, no agent is created and the run starts once its researchers join, each job stating its own expected length.</InfoTip></>}
        >
          <Stack>
            <Segmented
              label="Baseline"
              size="sm"
              options={[{ value: 'measured', label: 'Measured by an agent' }, { value: 'stated', label: 'I\'ll give it' }]}
              value={baseline ? 'stated' : 'measured'}
              onChange={v => set('baseline', v === 'stated' ? {} : null)}
            />
            {baseline && ranked(form.metrics).map(m => (
              <FormField key={m.key} label={m.key}>
                <Input
                  type="number" className="w-40" step="any" value={baseline[m.key] ?? ''}
                  onChange={e => set('baseline', { ...baseline, [m.key]: e.target.value })}
                />
              </FormField>
            ))}
          </Stack>
        </FieldGroup>
      )}
      <FormField label="Duration (hours)" hint={locked ? LOCKED : 'Counted from the start.'}>
        <Input type="number" className="w-40" min={0} step="any" value={form.duration_hours} disabled={locked} onChange={e => set('duration_hours', e.target.value)} />
      </FormField>
    </>
  )
}
