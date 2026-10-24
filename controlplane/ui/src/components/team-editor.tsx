'use client'

import { useState } from 'react'
import { RESEARCHER_AGENT_TYPE, describeType, parseHyperparameters, teamTypes, typeLabel, useAgentRoles, useAgentTypes } from '@/lib/agent-types'
import { inheritedModel, useModels } from '@/lib/models'
import { agentTypesPath } from '@/lib/routes'
import type { TeamRow } from '@/lib/team'
import { useTenant } from '@/lib/tenant'
import { ModelPicker } from '@/components/model-picker'
import { Button } from '@/components/ui/button'
import { InsetCard } from '@/components/ui/card'
import { Input, Textarea } from '@/components/ui/input'
import { TextLink } from '@/components/ui/link'
import { SearchableSelect } from '@/components/ui/searchable-select'
import { Inline, Stack } from '@/components/ui/stack'
import { ErrorMessage, InlineError, Loading } from '@/components/ui/status-message'
import { Eyebrow, Pill, Text } from '@/components/ui/text'
import type { AgentRoleInfo, ListedAgentType } from '@/types/api'

function plural(n: number, name: string): string {
  return `${n} ${name.toLowerCase()}${n === 1 ? '' : 's'}`
}

function Entry({ n, row, type, role, types, roles, coordinatorModel, atWork, onChange, onRemove }: {
  n: number
  row: TeamRow
  type: ListedAgentType | undefined
  role: AgentRoleInfo | undefined
  types: ListedAgentType[]
  roles: AgentRoleInfo[]
  coordinatorModel: string | undefined
  atWork?: number
  onChange: (patch: Partial<TeamRow>) => void
  onRemove: () => void
}) {
  const [showHyperparameters, setShowHyperparameters] = useState(row.hyperparameters.trim() !== '')
  const parsed = parseHyperparameters(row.hyperparameters)
  const researcher = row.agent_type === RESEARCHER_AGENT_TYPE
  const { inherited, inheritedFrom } = inheritedModel(type?.current.settings.model, researcher, coordinatorModel)
  return (
    <InsetCard as="li" gap="sm" aria-label={`Team entry ${n}`}>
      <div className="grid grid-cols-[minmax(0,13.5rem)_3.5rem_minmax(0,1fr)_auto] items-end gap-x-2 gap-y-1">
        <Eyebrow>Agent type</Eyebrow>
        <Eyebrow>Count</Eyebrow>
        <Eyebrow>Model</Eyebrow>
        <span />
        <SearchableSelect
          aria-label={`Team entry ${n} type`}
          fieldSize="default"
          className="w-full min-w-0"
          hideAllOption
          value={row.agent_type}
          placeholder={row.agent_type}
          options={types.map(t => ({ value: t.id, label: typeLabel(t), group: roles.find(r => r.role === t.role)?.name, keywords: t.description }))}
          onChange={agent_type => onChange({ agent_type })}
        />
        <Input
          className="w-full min-w-0" type="number" min={1} step={1} value={row.count}
          aria-label={`Team entry ${n} count`}
          onChange={e => onChange({ count: e.target.value })}
        />
        <ModelPicker
          value={row.model}
          aria-label={`Team entry ${n} model`}
          inherited={inherited}
          inheritedFrom={inheritedFrom}
          coordinator={researcher}
          onChange={model => onChange({ model })}
        />
        <Button type="button" variant="ghost" size="icon" aria-label={`Remove team entry ${n}`} title="Remove this entry" onClick={onRemove}>×</Button>
      </div>
      <Inline gap="xs" align="baseline">
        {role && <Pill tone="accent" active>{role.name}</Pill>}
        <Text as="span" variant="hint">
          {type ? describeType(type) : `${row.agent_type} is no agent type a team may name.`}
          {atWork !== undefined && ` ${atWork} at work.`}
        </Text>
      </Inline>
      {showHyperparameters ? (
        <Stack gap="xs">
          <Eyebrow>Hyperparameters</Eyebrow>
          <Textarea
            mono rows={3}
            aria-label={`Team entry ${n} hyperparameters`}
            placeholder={'{"direction": "the approach these agents start from"}'}
            value={row.hyperparameters}
            invalid={'problem' in parsed}
            onChange={e => onChange({ hyperparameters: e.target.value })}
          />
          {'problem' in parsed
            ? <InlineError>{parsed.problem}</InlineError>
            : <Text variant="hint">A JSON object every agent of the entry starts from, the direction it is given. Empty is its agent type&apos;s.</Text>}
        </Stack>
      ) : (
        <span>
          <Button type="button" variant="ghost" size="sm" onClick={() => setShowHyperparameters(true)}>+ Hyperparameters</Button>
        </span>
      )}
    </InsetCard>
  )
}

/**
 * The team of an experiment as one list of entries, each an agent type with how many of it, the
 * model they run on and the hyperparameters they start from. The type gives the role, as the API
 * lists roles. An entry naming no model runs on its type's, else, a researcher's, on the
 * coordinator's. coordinatorModel is the experiment's own, empty for the platform's.
 */
export function TeamEditor({ rows, onChange, coordinatorModel, maxAgents, atWork }: {
  rows: TeamRow[]
  onChange: (rows: TeamRow[]) => void
  coordinatorModel: string
  /** The most executors at work at once, which the entries' executors may not exceed. */
  maxAgents?: number
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

  // Adding one of a role raises the count of an entry of its built-in type on the inherited model,
  // since two entries of one type and model are one entry.
  function add(role: AgentRoleInfo) {
    const same = rows.findIndex(r => r.agent_type === role.agent_type && r.model.trim() === '')
    if (same >= 0) return setRow(same, { count: String((Number(rows[same].count) || 0) + 1) })
    onChange([...rows, { agent_type: role.agent_type, count: '1', model: '', hyperparameters: '' }])
  }

  if (error) return <ErrorMessage>Could not load agent types: {error.message}</ErrorMessage>
  if (rolesError) return <ErrorMessage>Could not load the agent roles: {rolesError.message}</ErrorMessage>
  if (!catalog || !roles) return <Loading rows={2} />

  const totals = teamRoles.map(r => ({ role: r, n: rows.filter(row => roleOfRow(row)?.role === r.role).reduce((s, row) => s + (Number(row.count) || 0), 0) }))
  const executorTotal = totals.find(t => t.role.role === 'executor')
  const executors = executorTotal?.n ?? 0
  const over = maxAgents !== undefined && Number.isInteger(maxAgents) && executors > maxAgents

  return (
    <Stack gap="sm">
      {rows.length === 0
        ? <Text variant="note">No agents yet. The coordinator adds them while the run goes on.</Text>
        : (
          <Stack as="ol" gap="sm" aria-label="Team">
            {rows.map((row, i) => (
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
                onChange={patch => setRow(i, patch)}
                onRemove={() => onChange(rows.filter((_, idx) => idx !== i))}
              />
            ))}
          </Stack>
        )}
      <Inline className="justify-between">
        <Inline gap="xs">
          {teamRoles.map(r => (
            <Button key={r.role} type="button" size="sm" title={r.description} onClick={() => add(r)}>+ Add {r.name.toLowerCase()}</Button>
          ))}
        </Inline>
        <TextLink href={agentTypesPath(org.id)} target="_blank" className="text-xs">Manage agent types</TextLink>
      </Inline>
      <Text variant="note">
        {totals.map(t => plural(t.n, t.role.name)).join(', ')}
        {maxAgents !== undefined && Number.isInteger(maxAgents) && executorTotal && ` · at most ${plural(maxAgents, executorTotal.role.name)} at work at once`}
      </Text>
      {over && <InlineError>The team has more executors than the max at once: raise the max or lower a count.</InlineError>}
      {teamRoles.filter(r => !totals.some(t => t.role.role === r.role && t.n > 0)).map(r => (
        <Text key={r.role} variant="hint">No {r.name.toLowerCase()} in the team. {r.description}</Text>
      ))}
    </Stack>
  )
}
