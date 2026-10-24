'use client'

import { parseHyperparameters } from '@/lib/agent-types'
import { ModelName } from '@/components/model-name'
import { ModelPicker } from '@/components/model-picker'
import { Facts } from '@/components/ui/facts'
import { FieldGroup, FormField } from '@/components/ui/form-field'
import { Input, Textarea } from '@/components/ui/input'
import { Segmented } from '@/components/ui/segmented'
import { agentTypeSettingsEffortValues } from '@/types/api'
import type { AgentRole, AgentTypeSettings } from '@/types/api'

/** Settings as typed: max_subagents and hyperparameters as text. */
export interface SettingsDraft {
  model: string
  max_subagents: string
  effort: AgentTypeSettings['effort']
  hyperparameters: string
}

export function toDraft(s: AgentTypeSettings): SettingsDraft {
  const h = s.hyperparameters ?? {}
  return {
    model: s.model ?? '',
    max_subagents: String(s.max_subagents),
    effort: s.effort,
    hyperparameters: Object.keys(h).length === 0 ? '' : JSON.stringify(h).length <= 80 ? JSON.stringify(h) : JSON.stringify(h, null, 2),
  }
}

/** The settings a draft stands for, or the problems of each field that keep it from standing for any. */
export function fromDraft(d: SettingsDraft): { settings: AgentTypeSettings; problems: Partial<Record<keyof SettingsDraft, string>> } {
  const problems: Partial<Record<keyof SettingsDraft, string>> = {}
  const h = parseHyperparameters(d.hyperparameters)
  if ('problem' in h) problems.hyperparameters = h.problem
  return {
    settings: {
      model: d.model.trim(),
      max_subagents: Number(d.max_subagents),
      effort: d.effort,
      hyperparameters: 'value' in h ? h.value : {},
    },
    problems,
  }
}

export function SettingsForm({ role, draft, onChange }: { role: AgentRole; draft: SettingsDraft; onChange: (d: SettingsDraft) => void }) {
  const { problems } = fromDraft(draft)
  const set = (patch: Partial<SettingsDraft>) => onChange({ ...draft, ...patch })
  return (
    <div className="grid grid-cols-1 gap-x-6 gap-y-4 md:grid-cols-2">
      <FormField label="Model" hint="Empty runs on the platform's default.">
        <ModelPicker value={draft.model} onChange={model => set({ model })} />
      </FormField>
      <FieldGroup label="Effort" hint="How hard the model thinks.">
        <Segmented
          label="Effort"
          options={agentTypeSettingsEffortValues.map(e => ({ value: e, label: e }))}
          value={draft.effort}
          onChange={effort => set({ effort })}
        />
      </FieldGroup>
      <FormField label="Max subagents" hint="How many subagents run at once.">
        <Input type="number" min={1} step={1} value={draft.max_subagents} onChange={e => set({ max_subagents: e.target.value })} />
      </FormField>
      {role === 'executor' && (
        <FormField
          className="md:col-span-2"
          label="Hyperparameters"
          hint="What an executor starts from when it is given none. A JSON object."
          error={problems.hyperparameters}
        >
          <Textarea
            mono rows={4} spellCheck={false} className="text-xs"
            value={draft.hyperparameters} placeholder='{"lr": 0.001}' invalid={!!problems.hyperparameters}
            onChange={e => set({ hyperparameters: e.target.value })}
          />
        </FormField>
      )}
    </div>
  )
}

/** Settings as read, for a type nobody here edits. */
export function SettingsFacts({ role, settings }: { role: AgentRole; settings: AgentTypeSettings }) {
  const h = settings.hyperparameters ?? {}
  return (
    <Facts facts={[
      ['Model', settings.model ? <ModelName key="model" model={settings.model} /> : 'platform default'],
      ['Effort', settings.effort],
      ['Max subagents', settings.max_subagents],
      ...(role === 'executor' ? [['Hyperparameters', Object.keys(h).length ? JSON.stringify(h) : 'none'] as [string, string]] : []),
    ]} />
  )
}
