import { useBudgetReference, useCapacity } from '@/lib/capacity'
import { acceleratorLabel } from '@/lib/format'
import { budgetUnit } from '@/lib/units'
import { quotaTierValues } from '@/types/api'
import type { QuotaTier, TypeKind } from '@/types/api'
import { BudgetHint } from '@/components/budget-hint'
import { Disclosure } from '@/components/ui/disclosure'
import { FieldGroup, FormField } from '@/components/ui/form-field'
import { InfoTip } from '@/components/ui/info-tip'
import { Input } from '@/components/ui/input'
import { Segmented } from '@/components/ui/segmented'
import { Select } from '@/components/ui/select'
import { Inline, Stack } from '@/components/ui/stack'
import { Pill } from '@/components/ui/text'
import { StagesEditor } from '@/components/platform-experiment-form/stages-editor'
import { TeamEditor } from '@/components/team-editor'
import { CPU_TYPE, LOCKED, ONE_STAGE } from '@/components/platform-experiment-form/model'
import type { Machine, StepProps } from '@/components/platform-experiment-form/model'

const PLATFORM_DEFAULT = "Empty is the platform's default."

/** The machine of the plan: a type the org's jobs can be placed on now, or CPU cores, and what one node asks for. */
function MachineFields({ machine, onChange, locked }: { machine: Machine; onChange: (machine: Machine) => void; locked: StepProps['locked'] }) {
  const { data: capacity } = useCapacity()
  const types = Array.from(new Set((capacity?.clusters ?? []).flatMap(c => c.accelerators.filter(a => a.kind === 'accelerator').map(a => a.accelerator_type))))
  if (machine.accelerator_type && machine.accelerator_type !== CPU_TYPE && !types.includes(machine.accelerator_type)) types.unshift(machine.accelerator_type)
  const fixed = locked('cpu')
  return (
    <FieldGroup label="Machine" hint={fixed ? LOCKED : <>What one node of each job asks for.<InfoTip>Changing it measures the baseline again on the new machine. A change between an accelerator and CPU cores changes what the budget counts, so state the budget again.</InfoTip></>}>
      <div className="grid grid-cols-2 gap-4 sm:grid-cols-4">
        <FormField label="Accelerator">
          <Select
            className="w-full"
            value={machine.accelerator_type === CPU_TYPE ? '' : machine.accelerator_type}
            disabled={locked('accelerator_type')}
            onChange={type => onChange({ ...machine, accelerator_type: type })}
            options={[{ value: '', label: 'CPU cores' }, ...types.map(type => ({ value: type, label: acceleratorLabel(type) }))]}
          />
        </FormField>
        {(['cpu', 'memory', 'storage'] as const).map(field => (
          <FormField key={field} label={{ cpu: 'CPU', memory: 'Memory', storage: 'Storage' }[field]}>
            <Input value={machine[field]} disabled={locked(field)} onChange={e => onChange({ ...machine, [field]: e.target.value })} />
          </FormField>
        ))}
      </div>
    </FieldGroup>
  )
}

const tierLabel = (tier: QuotaTier) => tier.charAt(0).toUpperCase() + tier.slice(1).replace(/_/g, ' ')

/** Step 3: the team, budget, the stage ladder, and the settings most leave alone. */
export function StepBudget({ form, set, isEdit, locked, budgetKind }: StepProps & { budgetKind?: TypeKind }) {
  const reference = useBudgetReference()
  return (
    <>
      {!isEdit && (
        <FieldGroup
          label="Team"
          hint={<>The platform starts these with the experiment and keeps them up.<InfoTip>It replaces an agent that finishes or loses its machine while time and budget allow. A cut lowers the count.</InfoTip></>}
        >
          <TeamEditor
            rows={form.agents}
            onChange={rows => set('agents', rows)}
            coordinatorModel={form.coordinator_model}
            coordinator={{ onModelChange: m => set('coordinator_model', m) }}
          />
        </FieldGroup>
      )}

      {isEdit && form.machine && <MachineFields machine={form.machine} onChange={machine => set('machine', machine)} locked={locked} />}

      <div className="grid grid-cols-2 gap-4">
        <FormField
          label={`Budget (${budgetUnit(budgetKind ?? 'accelerator', 'long', reference)})`}
          hint={locked('budget_accelerator_hours') ? LOCKED : <BudgetHint budget={form.budget_accelerator_hours} kind={budgetKind} />}
        >
          <Input type="number" min={1} step={0.5} value={form.budget_accelerator_hours} disabled={locked('budget_accelerator_hours')} onChange={e => set('budget_accelerator_hours', e.target.value)} />
        </FormField>
      </div>

      <FieldGroup
        label="Stage ladder"
        hint={isEdit ? 'Fixed at creation.' : <>Each stage ends by cutting the weakest share of agents.<InfoTip>Lengths add up to 100% and the final stage cuts nobody. A run of fewer than 5 executors has one stage, since nobody is cut. Max job hours caps how long one job runs in a stage.</InfoTip></>}
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
          {!isEdit && (
            <FieldGroup label="Default quota tier" hint="For agents that join without stating one.">
              <Segmented
                label="Default quota tier"
                size="sm"
                options={[{ value: '' as const, label: 'Platform default' }, ...quotaTierValues.map(tier => ({ value: tier, label: tierLabel(tier) }))]}
                value={form.default_quota_tier}
                onChange={v => set('default_quota_tier', v)}
              />
            </FieldGroup>
          )}
          {isEdit && (
            <FormField label="Starts at" hint={locked('starts_at') ? LOCKED : 'When its baseline begins to be measured. Its baseline deadline moves with it.'}>
              <Input type="datetime-local" value={form.starts_at} disabled={locked('starts_at')} onChange={e => set('starts_at', e.target.value)} />
            </FormField>
          )}
          <FormField label="Max concurrent accelerators" hint={locked('max_concurrent_accelerators') ? LOCKED : `Held at once across its jobs. ${PLATFORM_DEFAULT}`}>
            <Input type="number" min={1} step={1} placeholder="platform default" value={form.max_concurrent_accelerators} disabled={locked('max_concurrent_accelerators')} onChange={e => set('max_concurrent_accelerators', e.target.value)} />
          </FormField>
          <FormField label="Report interval (seconds)" hint={locked('report_interval_seconds') ? LOCKED : `How often jobs report metrics. ${PLATFORM_DEFAULT}`}>
            <Input type="number" min={1} step={1} placeholder="platform default" value={form.report_interval_seconds} disabled={locked('report_interval_seconds')} onChange={e => set('report_interval_seconds', e.target.value)} />
          </FormField>
        </Stack>
      </Disclosure>
    </>
  )
}
