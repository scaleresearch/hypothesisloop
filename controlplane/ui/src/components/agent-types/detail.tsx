'use client'

import { useState } from 'react'
import { useRouter } from 'next/navigation'
import useSWR, { useSWRConfig } from 'swr'
import { ApiError, archiveAgentType, editAgentType, fetchAgentType } from '@/lib/api'
import { REFUSALS, describeType, nameProblem, promptProblems, roleOf } from '@/lib/agent-types'
import { relTime } from '@/lib/format'
import { agentTypesPath } from '@/lib/routes'
import { useTenant } from '@/lib/tenant'
import type { AgentType, EditedAgentType } from '@/types'
import { useCreateAgentType } from '@/components/agent-types/duplicate'
import { liveAgents } from '@/components/agent-types/list'
import { PromptEditor } from '@/components/agent-types/prompt-editor'
import { SettingsFacts, SettingsForm, fromDraft, toDraft } from '@/components/agent-types/settings-form'
import type { SettingsDraft } from '@/components/agent-types/settings-form'
import { VersionHistory } from '@/components/agent-types/versions'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Dialog } from '@/components/ui/dialog'
import { FormField } from '@/components/ui/form-field'
import { Input } from '@/components/ui/input'
import { Page } from '@/components/ui/page'
import { Section } from '@/components/ui/section'
import { Inline, Stack } from '@/components/ui/stack'
import { Callout, ErrorMessage, Loading } from '@/components/ui/status-message'
import { Code, Text } from '@/components/ui/text'

function said(e: unknown): string {
  if (e instanceof ApiError && e.reason && REFUSALS[e.reason]) return `${REFUSALS[e.reason]} ${e.message}`
  return e instanceof Error ? e.message : String(e)
}

// Why the person cannot edit the type, or null when they can.
function readOnlyReason(t: AgentType, administers: boolean, orgName: string): string | null {
  if (t.builtin) return 'Built-in types are read-only for everyone. Duplicate it to get a copy you can edit.'
  if (t.archived_at) return `Archived ${relTime(t.archived_at)}. It takes no new agents and can no longer be edited. Duplicate it to start a new type from it.`
  if (!administers) return `Only an owner of ${orgName} can edit agent types. You can read this one.`
  return null
}

// JSON with every object's keys sorted, so two equal values read the same whatever their key order.
function canonical(v: unknown): string {
  return JSON.stringify(v, (_, x) => x && typeof x === 'object' && !Array.isArray(x)
    ? Object.fromEntries(Object.entries(x).sort(([a], [b]) => a.localeCompare(b)))
    : x)
}

function sameSettings(a: object, b: object): boolean {
  return canonical(a) === canonical(b)
}

function Saved({ edited, onDismiss }: { edited: EditedAgentType | 'renamed'; onDismiss: () => void }) {
  if (edited === 'renamed') {
    return <Callout tone="success">Renamed. A new name makes no new version.</Callout>
  }
  const moved = edited.propagation.moved ?? []
  const failed = edited.propagation.failed ?? []
  return (
    <Callout tone={failed.length > 0 ? 'warning' : 'success'}>
      <Stack gap="xs">
        <span>
          Saved as v{edited.current_version}.{' '}
          {moved.length > 0
            ? `${moved.length} live ${moved.length === 1 ? 'agent picks' : 'agents pick'} it up at their next turn.`
            : 'No live agent runs it now.'}
        </span>
        {failed.length > 0 && (
          <>
            <span>{failed.length} could not be moved and keep the version they had:</span>
            <ul className="list-disc pl-5 text-xs">
              {failed.map(f => (
                <li key={`${f.platform_experiment_id}/${f.agent_id ?? ''}`}>
                  <Code>{f.agent_id ?? 'researchers'}</Code> in <Code>{f.platform_experiment_id}</Code>{f.error && `: ${f.error}`}
                </li>
              ))}
            </ul>
          </>
        )}
        <div><Button variant="link" size="sm" onClick={onDismiss}>Dismiss</Button></div>
      </Stack>
    </Callout>
  )
}

function ArchiveDialog({ type, onClose, onArchived }: { type: AgentType; onClose: () => void; onArchived: () => Promise<unknown> }) {
  const { org } = useTenant()
  const [busy, setBusy] = useState(false)
  const [refusal, setRefusal] = useState<{ inUse: boolean; message: string } | null>(null)

  async function archive() {
    setBusy(true)
    setRefusal(null)
    try {
      await archiveAgentType(org.id, type.id)
      await onArchived()
      onClose()
    } catch (e) {
      setRefusal({ inUse: e instanceof ApiError && e.reason === 'agent_type_in_use', message: e instanceof Error ? e.message : String(e) })
    } finally {
      setBusy(false)
    }
  }

  return (
    <Dialog open onClose={onClose} title={`Archive ${type.name}?`} size="sm">
      <Stack>
        <Text variant="note">It takes no new agents and can no longer be edited. Agents already on it keep running. Nothing is deleted.</Text>
        {refusal && (
          <ErrorMessage>
            {refusal.inUse
              ? <>Not archived: an experiment that is not closed still uses it. Close those experiments or move them to another type, then try again. <span className="text-xs">({refusal.message})</span></>
              : refusal.message}
          </ErrorMessage>
        )}
        <Inline gap="xs" className="justify-end">
          <Button onClick={onClose}>{refusal ? 'Close' : 'Cancel'}</Button>
          {!refusal?.inUse && <Button variant="danger" loading={busy} onClick={() => void archive()}>Archive</Button>}
        </Inline>
      </Stack>
    </Dialog>
  )
}

function Editor({ type, editable, onSaved, onReload }: {
  type: AgentType
  editable: boolean
  onSaved: (edited: EditedAgentType) => Promise<unknown>
  onReload: () => Promise<unknown>
}) {
  const { org } = useTenant()
  const [name, setName] = useState(type.name)
  const [prompt, setPrompt] = useState(type.current.prompt)
  const [settingsDraft, setSettingsDraft] = useState<SettingsDraft>(() => toDraft(type.current.settings))
  const [confirming, setConfirming] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<{ message: string; conflict: boolean } | null>(null)

  const { settings, problems: settingsProblems } = fromDraft(settingsDraft)
  const newVersion = prompt !== type.current.prompt || !sameSettings(settings, { ...type.current.settings, model: type.current.settings.model ?? '', hyperparameters: type.current.settings.hyperparameters ?? {} })
  const dirty = newVersion || name.trim() !== type.name
  const problems = [nameProblem(name), ...promptProblems(type.role, prompt), ...Object.values(settingsProblems)].filter(Boolean)
  const r = roleOf(type.role)

  function discard() {
    setName(type.name)
    setPrompt(type.current.prompt)
    setSettingsDraft(toDraft(type.current.settings))
    setError(null)
  }

  async function save() {
    setConfirming(false)
    setBusy(true)
    setError(null)
    try {
      const edited = await editAgentType(org.id, type.id, { if_version: type.current_version, name: name.trim(), prompt, settings })
      await onSaved(edited)
    } catch (e) {
      setError({ message: said(e), conflict: e instanceof ApiError && e.reason === 'version_conflict' })
    } finally {
      setBusy(false)
    }
  }

  function trySave() {
    if (newVersion && type.live_agents > 0) setConfirming(true)
    else void save()
  }

  return (
    <>
      <Section title="Name and role">
        <div className="grid grid-cols-1 gap-x-6 gap-y-4 md:grid-cols-2">
          <FormField label="Name" error={editable ? nameProblem(name) : undefined}>
            <Input value={name} readOnly={!editable} onChange={e => setName(e.target.value)} />
          </FormField>
          <FormField label="Role" hint="Fixed once made.">
            <Input value={r.label} readOnly disabled />
          </FormField>
        </div>
      </Section>
      <Section title="Settings">
        {editable
          ? <SettingsForm role={type.role} draft={settingsDraft} onChange={setSettingsDraft} />
          : <SettingsFacts role={type.role} settings={type.current.settings} />}
      </Section>
      <Section title="Prompt" note={`v${type.current_version}`}>
        <PromptEditor role={type.role} value={prompt} onChange={setPrompt} readOnly={!editable} />
      </Section>

      {editable && (dirty || error) && (
        <div className="sticky bottom-0 z-10 -mx-1 rounded-lg border bg-surface-raised px-4 py-3 shadow-lg">
          <Stack gap="sm">
            {error && (
              <ErrorMessage>
                {error.message}
                {error.conflict && <> Reloading shows the newest version and drops your edits here.</>}
              </ErrorMessage>
            )}
            <Inline className="justify-between">
              <Text as="span" variant="note">
                {problems.length > 0
                  ? `${problems.length} ${problems.length === 1 ? 'problem' : 'problems'} to fix before saving.`
                  : newVersion
                    ? `Saving makes v${type.current_version + 1}.${type.live_agents > 0 ? ` ${type.live_agents} live ${type.live_agents === 1 ? 'agent moves' : 'agents move'} to it.` : ''}`
                    : 'Only the name changed: no new version.'}
              </Text>
              <Inline gap="xs">
                {error?.conflict && <Button size="sm" onClick={() => void onReload()}>Reload</Button>}
                <Button size="sm" onClick={discard} disabled={busy}>Discard</Button>
                <Button size="sm" variant="primary" loading={busy} disabled={problems.length > 0 || !dirty} onClick={trySave}>
                  {newVersion ? `Save as v${type.current_version + 1}` : 'Save'}
                </Button>
              </Inline>
            </Inline>
          </Stack>
        </div>
      )}

      {confirming && (
        <Dialog open onClose={() => setConfirming(false)} title={`Save as v${type.current_version + 1}?`} size="sm">
          <Stack>
            <Text variant="note">
              {type.live_agents} live {type.live_agents === 1 ? 'agent runs' : 'agents run'} this type.
              Each one restarts its session with the new version at its next turn. Its work so far stays.
            </Text>
            <Inline gap="xs" className="justify-end">
              <Button onClick={() => setConfirming(false)}>Cancel</Button>
              <Button variant="primary" onClick={() => void save()}>Save and restart sessions</Button>
            </Inline>
          </Stack>
        </Dialog>
      )}
    </>
  )
}

/** One agent type: what it is, its editor for an owner, and its versions. */
export function AgentTypeDetail({ typeID }: { typeID: string }) {
  const router = useRouter()
  const { org, administers } = useTenant()
  const { mutate } = useSWRConfig()
  const { data: type, error, mutate: mutateType } = useSWR(['agent-type', org.id, typeID], () => fetchAgentType(org.id, typeID))
  const { create, busy: duplicating, error: duplicateError } = useCreateAgentType(org.id)
  const [archiving, setArchiving] = useState(false)
  const [saved, setSaved] = useState<{ edited: EditedAgentType; renamed: boolean } | null>(null)

  async function refresh(next?: AgentType) {
    await mutateType(next)
    await mutate(['agent-types', org.id])
  }

  async function edited(t: EditedAgentType) {
    setSaved({ edited: t, renamed: t.current_version === type?.current_version })
    await refresh(t)
  }

  const back = { label: 'Agent types', onClick: () => router.push(agentTypesPath(org.id)) }
  if (error) {
    const missing = error instanceof ApiError && error.status === 404
    return <Page title="Agent type" back={back}><ErrorMessage>{missing ? `No agent type ${typeID} in ${org.name}.` : `Could not load the agent type: ${error.message}`}</ErrorMessage></Page>
  }
  if (!type) return <Page title="Agent type" back={back}><Loading /></Page>

  const reason = readOnlyReason(type, administers, org.name)
  return (
    <Page
      back={back}
      title={type.name}
      description={
        <Stack gap="xs">
          <Inline gap="sm">
            <Badge tone="accent">{roleOf(type.role).label}</Badge>
            {type.builtin && <Badge tone="neutral">Built-in, read-only</Badge>}
            {type.archived_at && <Badge tone="warning">Archived</Badge>}
            <Text as="span" variant="note">v{type.current_version} · {liveAgents(type.live_agents)}</Text>
            <Code className="text-2xs text-subtle">{type.id}</Code>
          </Inline>
          <span>{describeType(type)}</span>
        </Stack>
      }
      actions={administers && (
        <Inline gap="sm">
          <Button size="sm" loading={duplicating === type.id} onClick={() => void create({ from: type.id }, type.id)}>Duplicate</Button>
          {!type.builtin && !type.archived_at && <Button size="sm" variant="danger" onClick={() => setArchiving(true)}>Archive</Button>}
        </Inline>
      )}
    >
      {reason && <Callout tone="neutral">{reason}</Callout>}
      {duplicateError && <ErrorMessage>{duplicateError}</ErrorMessage>}
      {saved && <Saved edited={saved.renamed ? 'renamed' : saved.edited} onDismiss={() => setSaved(null)} />}
      <Editor key={`${type.id}:${type.current_version}`} type={type} editable={reason === null} onSaved={edited} onReload={() => refresh()} />
      <VersionHistory orgID={org.id} type={type} />
      {archiving && <ArchiveDialog type={type} onClose={() => setArchiving(false)} onArchived={() => refresh()} />}
    </Page>
  )
}
