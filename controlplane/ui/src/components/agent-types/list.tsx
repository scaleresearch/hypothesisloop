'use client'

import { useState } from 'react'
import { describeType, roleOf, useAgentRoles, useAgentTypes } from '@/lib/agent-types'
import { agentTypesPath } from '@/lib/routes'
import { useTenant } from '@/lib/tenant'
import { useCreateAgentType } from '@/components/agent-types/duplicate'
import { ModelName } from '@/components/model-name'
import { InfoTip } from '@/components/ui/info-tip'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Dialog } from '@/components/ui/dialog'
import { FormField } from '@/components/ui/form-field'
import { Input } from '@/components/ui/input'
import { TextLink } from '@/components/ui/link'
import { Page } from '@/components/ui/page'
import { Section } from '@/components/ui/section'
import { Segmented } from '@/components/ui/segmented'
import { Inline, Stack } from '@/components/ui/stack'
import { ErrorMessage, Loading } from '@/components/ui/status-message'
import { Table, num } from '@/components/ui/table'
import { Text } from '@/components/ui/text'
import type { AgentRole, AgentRoleInfo, ListedAgentType } from '@/types/api'

function SettingsLine({ t }: { t: ListedAgentType }) {
  const s = t.current.settings
  return <>{s.model ? <ModelName model={s.model} /> : 'platform default'} · {s.effort} effort · {s.max_subagents} subagents</>
}

function NewTypeDialog({ catalog, roles: allRoles, onClose }: { catalog: ListedAgentType[]; roles: AgentRoleInfo[]; onClose: () => void }) {
  const { org } = useTenant()
  const { create, busy, error } = useCreateAgentType(org.id)
  // A new type starts as a copy of its role's built-in one, so the roles offered are those the API lets the caller copy.
  const roles = allRoles.filter(r => r.org_types && catalog.some(t => t.builtin && t.id === r.agent_type && t.actions.includes('duplicate')))
  const [role, setRole] = useState<AgentRole>(roles[0]?.role ?? allRoles[0].role)
  const [name, setName] = useState('')
  const start = catalog.find(t => t.builtin && t.id === roleOf(allRoles, role).agent_type)

  async function submit(e: React.FormEvent) {
    e.preventDefault()
    if (!start) return
    await create({ from: start.id, name: name.trim() || undefined }, 'new')
  }

  return (
    <Dialog open onClose={onClose} title="New agent type" size="sm">
      <Stack as="form" gap="lg" onSubmit={submit}>
        <Stack gap="xs">
          <Segmented
            label="Role"
            size="sm"
            options={roles.map(r => ({ value: r.role, label: r.name }))}
            value={role}
            onChange={setRole}
          />
          <Text variant="hint">{roleOf(allRoles, role).description} The role is fixed once made.</Text>
        </Stack>
        <FormField label="Name">
          <Input value={name} placeholder={start ? `Copy of ${start.name}` : ''} onChange={e => setName(e.target.value)} autoFocus />
        </FormField>
        {error && <ErrorMessage>{error}</ErrorMessage>}
        <Inline gap="xs" className="justify-end">
          <Button type="button" onClick={onClose}>Cancel</Button>
          <Button type="submit" variant="primary" loading={busy === 'new'} disabled={!start}>Create and edit</Button>
        </Inline>
      </Stack>
    </Dialog>
  )
}

function RoleGroup({ r, types, duplicate, busy }: {
  r: AgentRoleInfo
  types: ListedAgentType[]
  duplicate: (t: ListedAgentType) => void
  busy: string | null
}) {
  const { org } = useTenant()
  const duplicates = types.some(t => t.actions.includes('duplicate'))
  // Built-in first in the platform's order, then the org's own by name, archived last.
  const ordered = [...types].sort((a, b) =>
    Number(b.builtin) - Number(a.builtin) || Number(!!a.archived_at) - Number(!!b.archived_at) || (a.builtin ? 0 : a.name.localeCompare(b.name)))
  return (
    <Section title={`${r.name} types`} note={r.description && <InfoTip>{r.description}</InfoTip>} scrollX>
      <Table aria-label={`${r.name} types`} className="min-w-3xl table-fixed">
        <thead>
          <tr>
            <th>Type</th><th className="w-64">Settings</th><th className="w-20 text-right">Version</th>
            {duplicates && <th className="w-28" />}
          </tr>
        </thead>
        <tbody>
          {ordered.map(t => (
            <tr key={t.id} className={t.archived_at ? 'opacity-60' : undefined}>
              <td className="max-w-md">
                <Inline gap="xs">
                  <TextLink href={agentTypesPath(org.id, t.id)} className="text-sm font-medium">{t.name}</TextLink>
                  {t.builtin && <Badge tone="neutral" title="Seeded by the platform. Duplicate it to change it.">Built-in, read-only</Badge>}
                  {t.archived_at && <Badge tone="warning">Archived</Badge>}
                </Inline>
                <Text variant="hint" className="mt-0.5">{describeType(t)}</Text>
              </td>
              <td className="whitespace-nowrap text-xs text-muted"><SettingsLine t={t} /></td>
              <td className={num}>v{t.current_version}</td>
              {duplicates && (
                <td className="text-right">
                  {t.actions.includes('duplicate') && <Button size="sm" loading={busy === t.id} onClick={() => duplicate(t)}>Duplicate</Button>}
                </td>
              )}
            </tr>
          ))}
        </tbody>
      </Table>
    </Section>
  )
}

/** Every agent type of the org, grouped by role, with duplicate and new for its owners. */
export function AgentTypesList() {
  const { org } = useTenant()
  const editsOrg = org.actions.includes('edit')
  const { data: catalog, error } = useAgentTypes(org.id)
  const { data: roles, error: rolesError } = useAgentRoles()
  const { create, busy, error: createError } = useCreateAgentType(org.id)
  const [adding, setAdding] = useState(false)

  return (
    <Page
      title="Agent types"
      actions={editsOrg && catalog && roles && <Button variant="primary" size="sm" onClick={() => setAdding(true)}>New type</Button>}
    >
      {error && <ErrorMessage>Could not load agent types: {error.message}</ErrorMessage>}
      {rolesError && <ErrorMessage>Could not load the agent roles: {rolesError.message}</ErrorMessage>}
      {createError && <ErrorMessage>{createError}</ErrorMessage>}
      {(!catalog || !roles) && !error && !rolesError && <Loading />}
      {catalog && roles?.map(r => {
        const types = catalog.filter(t => t.role === r.role)
        return types.length > 0 && (
          <RoleGroup
            key={r.role}
            r={r}
            types={types}
            busy={busy}
            duplicate={t => void create({ from: t.id }, t.id)}
          />
        )
      })}
      {adding && catalog && roles && <NewTypeDialog catalog={catalog} roles={roles} onClose={() => setAdding(false)} />}
    </Page>
  )
}
