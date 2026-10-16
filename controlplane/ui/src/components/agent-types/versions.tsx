'use client'

import { useState } from 'react'
import { clsx } from 'clsx'
import useSWR from 'swr'
import { fetchAgentTypeVersions, restoreAgentType } from '@/lib/api/agent-types'
import type { AgentType, AgentTypeSettings, AgentTypeVersion, EditedAgentType } from '@/types/agent-types'
import { APPLIES_LIVE, said } from '@/lib/agent-types'
import { diffLines, foldUnchanged } from '@/lib/diff'
import type { DiffLine } from '@/lib/diff'
import { relTime } from '@/lib/format'
import { Button } from '@/components/ui/button'
import { Dialog } from '@/components/ui/dialog'
import { EmptyState } from '@/components/ui/empty-state'
import { Section } from '@/components/ui/section'
import { Select } from '@/components/ui/select'
import { Inline, Stack } from '@/components/ui/stack'
import { ErrorMessage, Loading } from '@/components/ui/status-message'
import { Eyebrow, Text } from '@/components/ui/text'
import { keys } from '@/lib/keys'

const lineClass: Record<DiffLine['kind'], string> = {
  same: 'text-muted',
  add: 'bg-success-faint text-foreground',
  del: 'bg-danger-faint text-foreground',
}

const mark: Record<DiffLine['kind'], string> = { same: ' ', add: '+', del: '-' }

function Lines({ lines }: { lines: DiffLine[] }) {
  return (
    <>
      {lines.map((l, i) => (
        <div key={i} className={clsx('grid grid-cols-[2.5rem_2.5rem_1rem_minmax(0,1fr)]', lineClass[l.kind])}>
          <span className="select-none pr-2 text-right text-subtle">{l.before ?? ''}</span>
          <span className="select-none pr-2 text-right text-subtle">{l.after ?? ''}</span>
          <span className="select-none" aria-hidden="true">{mark[l.kind]}</span>
          <span className="whitespace-pre-wrap break-words">
            {l.kind !== 'same' && <span className="sr-only">{l.kind === 'add' ? 'Added: ' : 'Removed: '}</span>}
            {l.text || ' '}
          </span>
        </div>
      ))}
    </>
  )
}

/** The lines of two prompts, changed ones marked and long unchanged stretches folded. */
function PromptDiff({ before, after }: { before: string; after: string }) {
  const [opened, setOpened] = useState<Set<number>>(new Set())
  const lines = diffLines(before, after)
  if (lines.every(l => l.kind === 'same')) return <Text variant="note">The prompt is the same.</Text>
  const added = lines.filter(l => l.kind === 'add').length
  const removed = lines.filter(l => l.kind === 'del').length
  return (
    <Stack gap="xs">
      <Text variant="note"><span className="text-success">+{added}</span> <span className="text-danger">-{removed}</span> lines</Text>
      <div className="overflow-hidden rounded border bg-surface font-mono text-2xs leading-relaxed" aria-label="Prompt diff">
        {foldUnchanged(lines).map((h, i) => h.kind === 'lines' || opened.has(i)
          ? <Lines key={i} lines={h.lines} />
          : (
            <button
              key={i}
              type="button"
              onClick={() => setOpened(new Set(opened).add(i))}
              className="block w-full border-y bg-surface-inset px-3 py-1 text-left text-subtle hover:text-foreground"
            >
              Show {h.lines.length} unchanged lines
            </button>
          ))}
      </div>
    </Stack>
  )
}

function show(v: unknown): string {
  if (v === undefined || v === '') return 'none'
  if (typeof v === 'object' && v !== null && Object.keys(v).length === 0) return 'none'
  return typeof v === 'string' ? v : JSON.stringify(v)
}

/** The settings that differ between two versions, each from before to after. */
function settingsChanges(before: AgentTypeSettings, after: AgentTypeSettings): { field: string; from: string; to: string }[] {
  const fields: [keyof AgentTypeSettings, string][] = [
    ['model', 'Model'], ['effort', 'Effort'], ['max_subagents', 'Max subagents'], ['hyperparameters', 'Hyperparameters'],
  ]
  return fields
    .map(([key, field]) => ({ field, from: show(before[key]), to: show(after[key]) }))
    .map(c => c.field === 'Model' ? { ...c, from: c.from === 'none' ? 'platform default' : c.from, to: c.to === 'none' ? 'platform default' : c.to } : c)
    .filter(c => c.from !== c.to)
}

function VersionDiff({ before, after }: { before: AgentTypeVersion; after: AgentTypeVersion }) {
  const changes = settingsChanges(before.settings, after.settings)
  return (
    <Stack>
      <Stack gap="xs">
        <Eyebrow>Settings</Eyebrow>
        {changes.length === 0
          ? <Text variant="note">The settings are the same.</Text>
          : (
            <ul className="grid gap-0.5 text-xs">
              {changes.map(c => (
                <li key={c.field}>
                  <span className="text-muted">{c.field}:</span>{' '}
                  <span className="font-mono text-danger line-through">{c.from}</span>{' → '}
                  <span className="font-mono text-success">{c.to}</span>
                </li>
              ))}
            </ul>
          )}
      </Stack>
      <Stack gap="xs">
        <Eyebrow>Prompt</Eyebrow>
        <PromptDiff before={before.prompt} after={after.prompt} />
      </Stack>
    </Stack>
  )
}

function RestoreDialog({ orgID, type, version, onClose, onRestored }: {
  orgID: string
  type: AgentType
  version: number
  onClose: () => void
  onRestored: (t: EditedAgentType) => Promise<unknown>
}) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  async function restore() {
    setBusy(true)
    setError('')
    try {
      await onRestored(await restoreAgentType(orgID, type.id, version))
      onClose()
    } catch (e) {
      setError(said(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Dialog open onClose={onClose} title={`Restore v${version}?`} size="sm">
      <Stack>
        <Text variant="note">Its prompt and settings become v{type.current_version + 1}. Every version stays. {APPLIES_LIVE}</Text>
        {error && <ErrorMessage>{error}</ErrorMessage>}
        <Inline gap="xs" className="justify-end">
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" loading={busy} onClick={() => void restore()}>Restore</Button>
        </Inline>
      </Stack>
    </Dialog>
  )
}

/** Every version of an agent type, newest first, what changed between any two of them, and a
 * restore of an earlier one for an owner. */
export function VersionHistory({ orgID, type, editable, onRestored }: {
  orgID: string
  type: AgentType
  editable: boolean
  onRestored: (t: EditedAgentType) => Promise<unknown>
}) {
  const { data: versions, error } = useSWR(keys.agentTypeVersions(orgID, type.id, type.current_version), () => fetchAgentTypeVersions(orgID, type.id))
  const [from, setFrom] = useState<number | null>(null)
  const [to, setTo] = useState<number | null>(null)
  const [restoring, setRestoring] = useState<number | null>(null)

  if (error) return <Section title="Version history"><ErrorMessage>Could not load the versions: {error.message}</ErrorMessage></Section>
  if (!versions) return <Section title="Version history"><Loading /></Section>

  const newest = versions[0]?.version ?? type.current_version
  const after = versions.find(v => v.version === (to ?? newest))
  const before = versions.find(v => v.version === (from ?? Math.max(1, (to ?? newest) - 1)))
  const options = versions.map(v => ({ value: String(v.version), label: `v${v.version}${v.version === type.current_version ? ' (current)' : ''}` }))

  return (
    <Section title="Version history" note={`${versions.length} ${versions.length === 1 ? 'version' : 'versions'}`}>
      <Stack gap="lg">
        <ol className="grid gap-1">
          {versions.map(v => (
            <li key={v.version} className="flex flex-wrap items-baseline gap-x-3 text-xs">
              <span className="w-8 font-mono font-medium">v{v.version}</span>
              <span className="text-muted" title={new Date(v.created_at).toLocaleString()}>
                {relTime(v.created_at)}{v.created_by ? ` by ${v.created_by.name}` : ' by the platform'}
              </span>
              {v.version === type.current_version && <span className="text-success">current</span>}
              <span className="ml-auto font-mono text-2xs text-subtle" title="sha256 of prompt and settings">{v.digest.slice(0, 12)}</span>
              {versions.length > 1 && v.version > 1 && (
                <Button variant="link" size="sm" onClick={() => { setFrom(v.version - 1); setTo(v.version) }}>Changes</Button>
              )}
              {editable && v.version !== type.current_version && (
                <Button variant="link" size="sm" onClick={() => setRestoring(v.version)}>Restore</Button>
              )}
            </li>
          ))}
        </ol>
        {versions.length < 2 ? (
          <EmptyState>One version so far.</EmptyState>
        ) : (
          <Stack>
            <Inline gap="sm">
              <Text as="span" variant="note">Compare</Text>
              <Select aria-label="Compare from" fieldSize="sm" className="w-36" value={String(before?.version ?? '')} options={options} onChange={v => setFrom(Number(v))} />
              <Text as="span" variant="note">with</Text>
              <Select aria-label="Compare with" fieldSize="sm" className="w-36" value={String(after?.version ?? '')} options={options} onChange={v => setTo(Number(v))} />
            </Inline>
            {before && after && <VersionDiff before={before} after={after} />}
          </Stack>
        )}
      </Stack>
      {restoring !== null && (
        <RestoreDialog orgID={orgID} type={type} version={restoring} onClose={() => setRestoring(null)} onRestored={onRestored} />
      )}
    </Section>
  )
}
