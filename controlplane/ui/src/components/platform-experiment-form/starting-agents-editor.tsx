import { Fragment } from 'react'
import { describeType, researcherTypes, useAgentTypes } from '@/lib/agent-types'
import { agentTypesPath } from '@/lib/routes'
import { useTenant } from '@/lib/tenant'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { TextLink } from '@/components/ui/link'
import { Select } from '@/components/ui/select'
import { Inline, Stack } from '@/components/ui/stack'
import { ErrorMessage, Loading } from '@/components/ui/status-message'
import { Eyebrow, Text } from '@/components/ui/text'
import type { StartingAgentDraft } from '@/components/platform-experiment-form/model'

/** The agents a new experiment starts with, one row per agent type and model. */
export function StartingAgentsEditor({ rows, onChange }: { rows: StartingAgentDraft[]; onChange: (rows: StartingAgentDraft[]) => void }) {
  const { org } = useTenant()
  const { data: catalog, error } = useAgentTypes(org.id)
  const types = researcherTypes(catalog)
  // A row added is of a type no row names yet, since two rows of one type and model are one row.
  const unused = types.find(t => !rows.some(r => r.agent_type === t.id && r.model.trim() === ''))

  function setRow(i: number, patch: Partial<StartingAgentDraft>) {
    onChange(rows.map((r, idx) => idx === i ? { ...r, ...patch } : r))
  }

  if (error) return <ErrorMessage>Could not load agent types: {error.message}</ErrorMessage>
  if (!catalog) return <Loading rows={2} />

  return (
    <Stack gap="sm">
      <div className="grid grid-cols-[minmax(0,2fr)_5rem_minmax(0,2fr)_auto] items-center gap-x-2 gap-y-1">
        <Eyebrow>Type</Eyebrow>
        <Eyebrow>Count</Eyebrow>
        <Eyebrow>Model</Eyebrow>
        <span />
        {rows.map((r, i) => {
          const type = catalog.find(t => t.id === r.agent_type)
          return (
            <Fragment key={i}>
              <Select
                aria-label={`Starting agent ${i + 1} type`}
                value={r.agent_type}
                options={types.map(t => ({ value: t.id, label: t.builtin ? t.name : `${t.name} (v${t.current_version})` }))}
                placeholder={r.agent_type}
                onChange={agent_type => setRow(i, { agent_type })}
              />
              <Input
                className="w-full min-w-0" type="number" min={1} step={1} value={r.count}
                aria-label={`Starting agent ${i + 1} count`}
                onChange={e => setRow(i, { count: e.target.value })}
              />
              <Input
                className="w-full min-w-0" value={r.model}
                aria-label={`Starting agent ${i + 1} model`}
                placeholder={type?.current.settings.model || 'platform default'}
                onChange={e => setRow(i, { model: e.target.value })}
              />
              <Button
                type="button" variant="ghost" size="icon" className="size-6"
                onClick={() => onChange(rows.filter((_, idx) => idx !== i))}
                disabled={rows.length <= 1}
                aria-label={`Remove starting agent ${i + 1}`}
                title="Remove this row"
              >×</Button>
              <Text variant="hint" className="col-span-4 mb-1">
                {type ? describeType(type) : `${r.agent_type} is not a researcher type of ${org.name}.`}
              </Text>
            </Fragment>
          )
        })}
      </div>
      <Inline className="justify-between">
        <Button type="button" size="sm" disabled={!unused} title={unused ? undefined : 'Every agent type has a row: raise a count, or set a model to start one type on another model.'} onClick={() => unused && onChange([...rows, { agent_type: unused.id, count: '1', model: '' }])}>+ Add agents</Button>
        <TextLink href={agentTypesPath(org.id)} target="_blank" className="text-xs">Manage agent types</TextLink>
      </Inline>
    </Stack>
  )
}
