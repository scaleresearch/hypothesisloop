'use client'

import { useState } from 'react'
import { ROLES, describeType, roleOf, useAgentTypes } from '@/lib/agent-types'
import { agentTypesPath } from '@/lib/routes'
import { useTenant } from '@/lib/tenant'
import type { AgentRole, AgentType } from '@/types'
import { useCreateAgentType } from '@/components/agent-types/duplicate'
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

/** Live agents of a type in words, for a cell and a heading. */
export function liveAgents(n: number): string {
  return n === 0 ? 'Not in use' : `Used by ${n} live ${n === 1 ? 'agent' : 'agents'}`
}

function settingsLine(t: AgentType): string {
  const s = t.current.settings
  return `${s.model || 'platform default'} · ${s.effort} effort · ${s.max_subagents} subagents`
}

function NewTypeDialog({ catalog, onClose }: { catalog: AgentType[]; onClose: () => void }) {
  const { org } = useTenant()
  const { create, busy, error } = useCreateAgentType(org.id)
  const [role, setRole] = useState<AgentRole>('researcher')
  const [name, setName] = useState('')
  const start = catalog.find(t => t.builtin && t.id === roleOf(role).builtin)

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
            options={ROLES.map(r => ({ value: r.role, label: r.label }))}
            value={role}
            onChange={setRole}
          />
          <Text variant="hint">{roleOf(role).description} The role is fixed once made.</Text>
        </Stack>
        <FormField label="Name" hint={start ? `Starts as a copy of ${start.name}, which you then edit.` : undefined}>
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

function RoleGroup({ role, types, administers, duplicate, busy }: {
  role: AgentRole
  types: AgentType[]
  administers: boolean
  duplicate: (t: AgentType) => void
  busy: string | null
}) {
  const { org } = useTenant()
  const r = roleOf(role)
  // Built-in first in the platform's order, then the org's own by name, archived last.
  const ordered = [...types].sort((a, b) =>
    Number(b.builtin) - Number(a.builtin) || Number(!!a.archived_at) - Number(!!b.archived_at) || (a.builtin ? 0 : a.name.localeCompare(b.name)))
  return (
    <Section title={r.plural} note={r.description} scrollX>
      <Table aria-label={`${r.label} types`} className="min-w-3xl table-fixed">
        <thead>
          <tr>
            <th>Type</th><th className="w-64">Settings</th><th className="w-20 text-right">Version</th><th className="w-44">In use</th>
            {administers && <th className="w-28" />}
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
              <td className="whitespace-nowrap text-xs text-muted">{settingsLine(t)}</td>
              <td className={num}>v{t.current_version}</td>
              <td className="whitespace-nowrap text-xs">{liveAgents(t.live_agents)}</td>
              {administers && (
                <td className="text-right">
                  <Button size="sm" loading={busy === t.id} onClick={() => duplicate(t)}>Duplicate</Button>
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
  const { org, administers } = useTenant()
  const { data: catalog, error } = useAgentTypes(org.id)
  const { create, busy, error: createError } = useCreateAgentType(org.id)
  const [adding, setAdding] = useState(false)

  return (
    <Page
      title="Agent types"
      description="What agents run as: a role, a prompt and settings. Built-in types are read-only. Duplicate one to make your own."
      actions={administers && catalog && <Button variant="primary" size="sm" onClick={() => setAdding(true)}>New type</Button>}
    >
      {!administers && (
        <Text tone="muted" role="note">
          You are a member of {org.name}. You can read every type. Only an owner can create, edit or archive them.
        </Text>
      )}
      {error && <ErrorMessage>Could not load agent types: {error.message}</ErrorMessage>}
      {createError && <ErrorMessage>{createError}</ErrorMessage>}
      {!catalog && !error && <Loading />}
      {catalog && ROLES.map(({ role }) => {
        const types = catalog.filter(t => t.role === role)
        return types.length > 0 && (
          <RoleGroup
            key={role}
            role={role}
            types={types}
            administers={administers}
            busy={busy}
            duplicate={t => void create({ from: t.id }, t.id)}
          />
        )
      })}
      {adding && catalog && <NewTypeDialog catalog={catalog} onClose={() => setAdding(false)} />}
    </Page>
  )
}
