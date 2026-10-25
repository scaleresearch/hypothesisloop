'use client'

import type { ReactNode } from 'react'
import { RESEARCHER_AGENT_TYPE, describeType, teamTypes, typeLabel, useAgentRoles, useAgentTypes } from '@/lib/agent-types'
import { inheritedModel, useModels } from '@/lib/models'
import { agentTypesPath } from '@/lib/routes'
import { executorCount, sameEntry, startsFrom } from '@/lib/team'
import type { TeamRow } from '@/lib/team'
import { useTenant } from '@/lib/tenant'
import { ModelPicker } from '@/components/model-picker'
import { Button } from '@/components/ui/button'
import { InsetCard } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { TextLink } from '@/components/ui/link'
import { SearchableSelect } from '@/components/ui/searchable-select'
import { Inline, Stack } from '@/components/ui/stack'
import { ErrorMessage, InlineError, Loading } from '@/components/ui/status-message'
import { Eyebrow, Pill, Text } from '@/components/ui/text'
import type { AgentRoleInfo, ListedAgentType } from '@/types/api'

function plural(n: number, name: string): string {
  return `${n} ${name.toLowerCase()}${n === 1 ? '' : 's'}`
}

/** The columns of an entry: its type, count, model and remove button, the coordinator's alike. */
function EntryCard({ label, type, count, model, remove, role, description, children }: {
  label: string
  type: ReactNode
  count: ReactNode
  model: ReactNode
  remove?: ReactNode
  role: AgentRoleInfo | undefined
  description: string
  children?: ReactNode
}) {
  return (
    <InsetCard as="li" gap="sm" aria-label={label}>
      <div className="grid grid-cols-[minmax(0,13.5rem)_3.5rem_minmax(0,1fr)_1.5rem] items-end gap-x-2 gap-y-1">
        <Eyebrow>Agent type</Eyebrow>
        <Eyebrow>Count</Eyebrow>
        <Eyebrow>Model</Eyebrow>
        <span />
        {type}
        {count}
        {model}
        {remove ?? <span />}
      </div>
      <Inline gap="xs" align="baseline">
        {role && <Pill tone="accent" active>{role.name}</Pill>}
        <Text as="span" variant="hint">{description}</Text>
      </Inline>
      {children}
    </InsetCard>
  )
}

function Entry({ n, row, type, role, types, roles, coordinatorModel, atWork, same, manage, onChange, onRemove }: {
  n: number
  row: TeamRow
  type: ListedAgentType | undefined
  role: AgentRoleInfo | undefined
  types: ListedAgentType[]
  roles: AgentRoleInfo[]
  coordinatorModel: string | undefined
  atWork?: number
  /** The number of the earlier entry of this type and model, which the API takes as one. */
  same?: number
  manage: { label: string; onSelect: () => void }
  onChange: (patch: Partial<TeamRow>) => void
  onRemove: () => void
}) {
  const researcher = row.agent_type === RESEARCHER_AGENT_TYPE
  const { inherited, inheritedFrom } = inheritedModel(type?.current.settings.model, researcher, coordinatorModel)
  const start = startsFrom(row)
  return (
    <EntryCard
      label={`Team entry ${n}`}
      role={role}
      description={`${type ? describeType(type) : `${row.agent_type} is no agent type a team may name.`}${atWork !== undefined ? ` ${atWork} at work.` : ''}`}
      type={
        <SearchableSelect
          aria-label={`Team entry ${n} type`}
          fieldSize="default"
          className="w-full min-w-0"
          hideAllOption
          value={row.agent_type}
          placeholder={row.agent_type}
          options={types.map(t => ({ value: t.id, label: typeLabel(t), group: roles.find(r => r.role === t.role)?.name, keywords: t.description }))}
          action={manage}
          // A cleared field closes on no type: the entry keeps the one it had.
          onChange={agent_type => agent_type && onChange({ agent_type })}
        />
      }
      count={
        <Input
          className="w-full min-w-0" type="number" min={1} step={1} value={row.count}
          aria-label={`Team entry ${n} count`}
          onChange={e => onChange({ count: e.target.value })}
        />
      }
      model={
        <ModelPicker
          value={row.model}
          aria-label={`Team entry ${n} model`}
          inherited={inherited}
          inheritedFrom={inheritedFrom}
          coordinator={researcher}
          onChange={model => onChange({ model })}
        />
      }
      remove={<Button type="button" variant="ghost" size="icon" aria-label={`Remove team entry ${n}`} title="Remove this entry" onClick={onRemove}>×</Button>}
    >
      {start && <Text variant="hint" className="truncate" title={start}>Starts from: {start}</Text>}
      {same !== undefined && <InlineError>Entry {same} has this type and model. Raise its count instead.</InlineError>}
    </EntryCard>
  )
}

/**
 * The team of an experiment as one list of entries, each an agent type with how many of it and
 * the model they run on, after its coordinator, of whom there is one. The type gives the role, as
 * the API lists roles. An entry naming no model runs on its type's, else, a researcher's, on the
 * coordinator's. coordinatorModel is the experiment's own, empty for the platform's.
 */
export function TeamEditor({ rows, onChange, coordinatorModel, coordinator = {}, perAgent, atWork }: {
  rows: TeamRow[]
  onChange: (rows: TeamRow[]) => void
  coordinatorModel: string
  /** The coordinator's entry, shown first, its model editable with onModelChange. false for an experiment without one. */
  coordinator?: { onModelChange?: (model: string) => void } | false
  /** Each row is one agent with a direction of its own, so rows may repeat a type and model. */
  perAgent?: boolean
  /** How many agents of each row are at work, on a running experiment. */
  atWork?: (row: TeamRow) => number
}) {
  const { org } = useTenant()
  const { data: catalog, error } = useAgentTypes(org.id)
  const { data: roles, error: rolesError } = useAgentRoles()
  const { data: models } = useModels()
  const types = teamTypes(catalog, roles)
  const teamRoles = (roles ?? []).filter(r => r.in_team)
  const roleOfRow = (row: TeamRow) => {
    const t = catalog?.find(x => x.id === row.agent_type)
    return t && roles?.find(r => r.role === t.role)
  }

  function setRow(i: number, patch: Partial<TeamRow>) {
    onChange(rows.map((r, idx) => idx === i ? { ...r, ...patch } : r))
  }

  // Adding one of a role adds an entry of its first type the team has no entry of on the
  // inherited model. Where it has one of each, the built-in type's entry counts one more, since
  // the API takes one entry of a type and model.
  function add(role: AgentRoleInfo) {
    const free = types.find(t => t.role === role.role && !rows.some(r => r.agent_type === t.id && r.model.trim() === ''))
    if (free) return onChange([...rows, { agent_type: free.id, count: '1', model: '' }])
    const same = rows.findIndex(r => r.agent_type === role.agent_type && r.model.trim() === '')
    if (same >= 0) return setRow(same, { count: String((Number(rows[same].count) || 0) + 1) })
    onChange([...rows, { agent_type: role.agent_type, count: '1', model: '' }])
  }

  if (error) return <ErrorMessage>Could not load agent types: {error.message}</ErrorMessage>
  if (rolesError) return <ErrorMessage>Could not load the agent roles: {rolesError.message}</ErrorMessage>
  if (!catalog || !roles) return <Loading rows={2} />

  // Every agent but a researcher is an executor, as the API counts them.
  const totals = teamRoles.map(r => ({
    role: r,
    n: r.role === 'executor' ? executorCount(rows) : rows.filter(row => row.agent_type === r.agent_type).reduce((s, row) => s + (Number(row.count) || 0), 0),
  }))
  const manage = { label: 'Manage agent types', onSelect: () => window.open(agentTypesPath(org.id), '_blank', 'noopener') }
  const coordinatorRole = roles.find(r => r.role === 'coordinator')
  const coordinatorType = catalog.find(t => t.id === coordinatorRole?.agent_type)

  return (
    <Stack gap="sm">
      <Stack as="ol" gap="sm" aria-label="Team">
        {coordinator && (
          <EntryCard
            label="Coordinator"
            role={coordinatorRole}
            description={coordinatorRole?.description ?? ''}
            type={
              <SearchableSelect
                aria-label="Coordinator type"
                fieldSize="default"
                className="w-full min-w-0"
                hideAllOption
                disabled
                value={coordinatorType?.id ?? 'coordinator'}
                options={coordinatorType ? [{ value: coordinatorType.id, label: typeLabel(coordinatorType) }] : []}
                onChange={() => undefined}
              />
            }
            count={<Input className="w-full min-w-0" type="number" value="1" disabled aria-label="Coordinator count" readOnly />}
            model={
              <ModelPicker
                coordinator
                aria-label="Coordinator model"
                value={coordinatorModel}
                disabled={!coordinator.onModelChange}
                onChange={m => coordinator.onModelChange?.(m)}
              />
            }
          />
        )}
        {rows.map((row, i) => {
          const same = perAgent ? -1 : sameEntry(rows, i)
          return (
            <Entry
              key={i}
              n={i + 1}
              row={row}
              type={catalog.find(t => t.id === row.agent_type)}
              role={roleOfRow(row)}
              types={types}
              roles={roles}
              coordinatorModel={coordinatorModel || models?.coordinator_model}
              atWork={atWork?.(row)}
              same={same >= 0 ? same + 1 : undefined}
              manage={manage}
              onChange={patch => setRow(i, patch)}
              onRemove={() => onChange(rows.filter((_, idx) => idx !== i))}
            />
          )
        })}
      </Stack>
      {rows.length === 0 && <Text variant="note">No agents yet. The coordinator adds them while the run goes on.</Text>}
      <Inline className="justify-between">
        <Inline gap="xs">
          {teamRoles.map(r => (
            <Button key={r.role} type="button" size="sm" title={r.description} onClick={() => add(r)}>+ Add {r.name.toLowerCase()}</Button>
          ))}
        </Inline>
        <TextLink href={agentTypesPath(org.id)} target="_blank" className="text-xs">Manage agent types</TextLink>
      </Inline>
      <Text variant="note">{totals.map(t => plural(t.n, t.role.name)).join(', ')}</Text>
      {teamRoles.filter(r => !totals.some(t => t.role.role === r.role && t.n > 0)).map(r => (
        <Text key={r.role} variant="hint">No {r.name.toLowerCase()} in the team. {r.description}</Text>
      ))}
    </Stack>
  )
}
