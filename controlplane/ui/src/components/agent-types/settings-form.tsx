'use client'

import { EFFORTS, MAX_SUBAGENTS, modelProblem, parseHyperparameters } from '@/lib/agent-types'
import type { AgentRole, AgentTypeSettings } from '@/types'
import { Facts } from '@/components/ui/facts'
import { FieldGroup, FormField } from '@/components/ui/form-field'
import { Input, Textarea } from '@/components/ui/input'
import { Segmented } from '@/components/ui/segmented'
import { Select } from '@/components/ui/select'

/** Settings as typed: numbers as the select holds them, hyperparameters as JSON text. */
export interface SettingsDraft {
  model: string
  max_subagents: number
  effort: AgentTypeSettings['effort']
  hyperparameters: string
}

export function toDraft(s: AgentTypeSettings): SettingsDraft {
  const h = s.hyperparameters ?? {}
  return {
    model: s.model ?? '',
    max_subagents: s.max_subagents,
    effort: s.effort,
    hyperparameters: Object.keys(h).length === 0 ? '' : JSON.stringify(h).length <= 80 ? JSON.stringify(h) : JSON.stringify(h, null, 2),
  }
}

/** The settings a draft stands for, or the problems of each field that keep it from standing for any. */
export function fromDraft(d: SettingsDraft): { settings: AgentTypeSettings; problems: Partial<Record<keyof SettingsDraft, string>> } {
  const problems: Partial<Record<keyof SettingsDraft, string>> = {}
  const model = modelProblem(d.model.trim())
  if (model) problems.model = model
  const h = parseHyperparameters(d.hyperparameters)
  if ('problem' in h) problems.hyperparameters = h.problem
  return {
    settings: {
      model: d.model.trim(),
      max_subagents: d.max_subagents,
      effort: d.effort,
      hyperparameters: 'value' in h ? h.value : {},
    },
    problems,
  }
}

const counts = (max: number) => Array.from({ length: max }, (_, i) => ({ value: String(i + 1), label: String(i + 1) }))

export function SettingsForm({ role, draft, onChange }: { role: AgentRole; draft: SettingsDraft; onChange: (d: SettingsDraft) => void }) {
  const { problems } = fromDraft(draft)
  const set = (patch: Partial<SettingsDraft>) => onChange({ ...draft, ...patch })
  return (
    <div className="grid grid-cols-1 gap-x-6 gap-y-4 md:grid-cols-2">
      <FormField label="Model" hint="Empty runs on the platform's default." error={problems.model}>
        <Input mono value={draft.model} placeholder="platform default" invalid={!!problems.model} onChange={e => set({ model: e.target.value })} />
      </FormField>
      <FieldGroup label="Effort" hint="How hard the model thinks.">
        <Segmented
          label="Effort"
          options={EFFORTS.map(e => ({ value: e, label: e }))}
          value={draft.effort}
          onChange={effort => set({ effort })}
        />
      </FieldGroup>
      <FormField label="Max subagents" hint="How many subagents run at once.">
        <Select
          value={String(draft.max_subagents)}
          options={counts(MAX_SUBAGENTS)}
          onChange={v => set({ max_subagents: Number(v) })}
        />
      </FormField>
      {role === 'researcher' && (
        <FormField
          className="md:col-span-2"
          label="Hyperparameters"
          hint="What a researcher starts from when it is given none. A JSON object, at most 4 KB."
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
      ['Model', settings.model || 'platform default'],
      ['Effort', settings.effort],
      ['Max subagents', settings.max_subagents],
      ...(role === 'researcher' ? [['Hyperparameters', Object.keys(h).length ? JSON.stringify(h) : 'none'] as [string, string]] : []),
    ]} />
  )
}
