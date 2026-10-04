import { budgetUnit } from '@/lib/units'
import type { TypeKind } from '@/lib/units'
import { Disclosure } from '@/components/ui/disclosure'
import { FieldGroup, FormField } from '@/components/ui/form-field'
import { InfoTip } from '@/components/ui/info-tip'
import { Input } from '@/components/ui/input'
import { Segmented } from '@/components/ui/segmented'
import { Inline, Stack } from '@/components/ui/stack'
import { Pill } from '@/components/ui/text'
import { StagesEditor } from '@/components/platform-experiment-form/stages-editor'
import { LOCKED, ONE_STAGE } from '@/components/platform-experiment-form/model'
import type { StepProps } from '@/components/platform-experiment-form/model'

const PLATFORM_DEFAULT = "Empty is the platform's default."

/** Step 3: researchers, budget, the stage ladder, and the settings most leave alone. */
export function StepBudget({ form, set, isEdit, locked, budgetKind }: StepProps & { budgetKind?: TypeKind }) {
  const unit = budgetKind
    ? budgetUnit(budgetKind, 'long')
    : `${budgetUnit('accelerator', 'long')}, or ${budgetUnit('cpu', 'long')} for jobs on CPU`
  return (
    <>
      <div className="grid grid-cols-2 gap-4">
        <FormField label="Researchers" hint={locked ? LOCKED : isEdit ? 'Nobody joins once it has started.' : 'Run by the platform, with a coordinator.'}>
          <Input type="number" min={1} max={500} step={1} value={form.max_agents} disabled={locked} onChange={e => set('max_agents', e.target.value)} />
        </FormField>
        <FormField
          label="Budget (accelerator hours)"
          hint={locked ? LOCKED : <>Caps everything it spends.<InfoTip>{`Counted in ${unit}. An hour on an accelerator spends its type's price; memory and storage spend nothing.`}</InfoTip></>}
        >
          <Input type="number" min={1} step={0.5} value={form.budget_accelerator_hours} disabled={locked} onChange={e => set('budget_accelerator_hours', e.target.value)} />
        </FormField>
      </div>

      <FieldGroup
        label="Stage ladder"
        hint={isEdit ? 'Fixed at creation.' : <>Each stage ends by cutting the weakest share of agents.<InfoTip>Lengths add up to 100% and the final stage cuts nobody. Cuts are skipped at 4 agents or fewer. Max job hours caps how long one job runs in a stage.</InfoTip></>}
      >
        {isEdit ? (
          <Inline gap="xs">
            {form.stages
              ? form.stages.map((s, i) => (
                <Pill key={i}>
                  {i + 1}: {s.length_pct}% {Number(s.evict_pct) > 0 ? `→ cut ${s.evict_pct}%` : '→ no cut'}{s.max_job_hours ? ` · ≤${s.max_job_hours}h/job` : ''}
                </Pill>
              ))
              : <Pill>Platform default</Pill>}
          </Inline>
        ) : (
          <Stack>
            <Segmented
              label="Stage ladder"
              size="sm"
              options={[{ value: 'platform', label: 'Platform default' }, { value: 'custom', label: 'Custom' }]}
              value={form.stages ? 'custom' : 'platform'}
              onChange={v => set('stages', v === 'custom' ? ONE_STAGE : null)}
            />
            {form.stages && <StagesEditor stages={form.stages} onChange={s => set('stages', s)} />}
          </Stack>
        )}
      </FieldGroup>

      <Disclosure label="More settings">
        <Stack gap="lg">
          {!isEdit && (
            <FormField label="Agent image" hint="Empty is the platform's image; a custom one is built FROM it.">
              <Input value={form.agent_image} onChange={e => set('agent_image', e.target.value)} placeholder="registry.example.com/team/agent:tag" />
            </FormField>
          )}
          {isEdit && (
            <FormField label="Starts at" hint={locked ? LOCKED : 'When its wall clock starts. Its end and baseline deadline move with it.'}>
              <Input type="datetime-local" value={form.starts_at} disabled={locked} onChange={e => set('starts_at', e.target.value)} />
            </FormField>
          )}
          <FormField label="Max concurrent accelerators" hint={locked ? LOCKED : `Held at once across its jobs. ${PLATFORM_DEFAULT}`}>
            <Input type="number" min={1} step={1} placeholder="platform default" value={form.max_concurrent_accelerators} disabled={locked} onChange={e => set('max_concurrent_accelerators', e.target.value)} />
          </FormField>
          <FormField label="Report interval (seconds)" hint={locked ? LOCKED : `How often jobs report metrics. ${PLATFORM_DEFAULT}`}>
            <Input type="number" min={1} step={1} placeholder="platform default" value={form.report_interval_seconds} disabled={locked} onChange={e => set('report_interval_seconds', e.target.value)} />
          </FormField>
        </Stack>
      </Disclosure>
    </>
  )
}
