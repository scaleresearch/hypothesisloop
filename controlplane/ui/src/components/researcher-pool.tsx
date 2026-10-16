'use client'

import { Fragment, useState } from 'react'
import useSWR, { useSWRConfig } from 'swr'
import { fetchStages, updatePlatformExperiment } from '@/lib/api'
import type { PlatformExperimentPatch } from '@/lib/api'
import { DEFAULT_AGENT_TYPE, describeType, researcherTypes, typeName, useAgentTypes } from '@/lib/agent-types'
import { relTime } from '@/lib/format'
import { useReadOnly } from '@/lib/read-only'
import {
  atWorkByKey, cutByMax, poolKey, poolProblem, researchersAtWork, toPool, toRows,
} from '@/lib/researcher-pool'
import type { PoolRow } from '@/lib/researcher-pool'
import { useRoster } from '@/lib/roster'
import { agentTypesPath } from '@/lib/routes'
import { useTenant } from '@/lib/tenant'
import { PlatformExperimentStatus } from '@/types'
import type { JoinedAgent, PlatformExperiment } from '@/types'
import { Button } from '@/components/ui/button'
import { Dialog } from '@/components/ui/dialog'
import { FormField } from '@/components/ui/form-field'
import { Input } from '@/components/ui/input'
import { TextLink } from '@/components/ui/link'
import { Section } from '@/components/ui/section'
import { Select } from '@/components/ui/select'
import { Inline, Stack } from '@/components/ui/stack'
import { ErrorMessage, Loading } from '@/components/ui/status-message'
import { Table, num } from '@/components/ui/table'
import { Code, Eyebrow, Text } from '@/components/ui/text'

const NEW_ROW: PoolRow = { agent_type: DEFAULT_AGENT_TYPE, model: '', count: '1' }

function plural(n: number, one: string, many = `${one}s`): string {
  return `${n} ${n === 1 ? one : many}`
}

function CutConfirm({ max, cut, nameOf, onConfirm, onCancel }: {
  max: number
  cut: JoinedAgent[]
  nameOf: (typeID: string) => string
  onConfirm: () => void
  onCancel: () => void
}) {
  return (
    <Dialog open onClose={onCancel} title={`Lower max researchers to ${max}?`} size="sm">
      <Stack>
        <Text variant="note">
          This cuts the {cut.length === 1 ? 'newest researcher' : `${cut.length} newest researchers`} at work.
          {cut.length === 1 ? ' It winds down' : ' Each winds down'} as a cut researcher does, and nobody takes {cut.length === 1 ? 'its' : 'their'} place.
        </Text>
        <ul className="grid gap-1 rounded border bg-surface-inset px-3 py-2 text-xs" aria-label="Researchers to cut">
          {cut.map(a => (
            <li key={a.agent_id} className="flex items-baseline justify-between gap-3">
              <span className="whitespace-nowrap font-medium">{a.name}</span>
              <span className="text-right text-muted">{a.flavor ? nameOf(a.flavor) : 'default'} · joined {relTime(a.signed_up_at)}</span>
            </li>
          ))}
        </ul>
        <Inline gap="xs" className="justify-end">
          <Button onClick={onCancel}>Cancel</Button>
          <Button variant="danger" onClick={onConfirm}>Lower and cut {cut.length}</Button>
        </Inline>
      </Stack>
    </Dialog>
  )
}

function PoolEditor({ pe, atWork, onDone }: { pe: PlatformExperiment; atWork: JoinedAgent[]; onDone: () => void }) {
  const { org } = useTenant()
  const { mutate } = useSWRConfig()
  const { data: catalog } = useAgentTypes(org.id)
  const types = researcherTypes(catalog)
  const [max, setMax] = useState(String(pe.max_agents))
  const [rows, setRows] = useState<PoolRow[]>(() => toRows(pe.researchers))
  const [confirming, setConfirming] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  const opened = JSON.stringify(toRows(pe.researchers))
  const poolChanged = JSON.stringify(rows) !== opened
  const maxN = Number(max)
  const maxChanged = maxN !== pe.max_agents
  const running = pe.status === PlatformExperimentStatus.RUNNING
  const cut = running && Number.isInteger(maxN) ? cutByMax(atWork, maxN) : []
  const remaining = atWork.slice(0, atWork.length - cut.length)
  const problem = poolProblem(maxN, poolChanged ? rows : null, remaining, id => typeName(id, catalog))
  const total = rows.reduce((s, r) => s + (Number(r.count) || 0), 0)

  function setRow(i: number, patch: Partial<PoolRow>) {
    setRows(rows.map((r, idx) => idx === i ? { ...r, ...patch } : r))
  }

  async function save() {
    setConfirming(false)
    setBusy(true)
    setError('')
    const patch: PlatformExperimentPatch = {}
    if (maxChanged) patch.max_agents = maxN
    if (poolChanged) patch.researchers = toPool(rows)
    try {
      const saved = await updatePlatformExperiment(pe.id, patch)
      await mutate(['pe', pe.id], saved, { revalidate: false })
      await mutate(['pe-agents', pe.id])
      onDone()
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Stack gap="lg">
      <FormField
        label="Max researchers"
        className="max-w-xs"
        hint={cut.length > 0
          ? `Saving cuts the ${plural(cut.length, 'newest researcher')} at work.`
          : `The most at work at once. ${atWork.length} at work now.`}
      >
        <Input type="number" min={1} step={1} value={max} onChange={e => setMax(e.target.value)} />
      </FormField>

      <Stack gap="sm">
        <Inline className="justify-between">
          <Eyebrow>Pool</Eyebrow>
          <TextLink href={agentTypesPath(org.id)} target="_blank" className="text-xs">Manage agent types</TextLink>
        </Inline>
        {rows.length === 0 && <Text variant="note">No pool. The coordinator adds researchers up to the max.</Text>}
        {rows.length > 0 && (
          <div className="grid grid-cols-[minmax(0,2fr)_minmax(0,2fr)_5rem_auto] items-center gap-x-2 gap-y-1">
            <Eyebrow>Type</Eyebrow>
            <Eyebrow>Model</Eyebrow>
            <Eyebrow>Count</Eyebrow>
            <span />
            {rows.map((r, i) => {
              const type = catalog?.find(t => t.id === r.agent_type)
              const working = atWorkByKey(remaining).get(poolKey(r.agent_type, r.model.trim())) ?? 0
              return (
                <Fragment key={i}>
                  <Select
                    aria-label={`Pool entry ${i + 1} type`}
                    value={r.agent_type}
                    placeholder={typeName(r.agent_type, catalog)}
                    options={types.map(t => ({ value: t.id, label: t.builtin ? t.name : `${t.name} (v${t.current_version})` }))}
                    onChange={agent_type => setRow(i, { agent_type })}
                  />
                  <Input
                    mono className="w-full min-w-0" value={r.model}
                    aria-label={`Pool entry ${i + 1} model`}
                    placeholder={type?.current.settings.model || 'platform default'}
                    onChange={e => setRow(i, { model: e.target.value })}
                  />
                  <Input
                    className="w-full min-w-0" type="number" min={Math.max(1, working)} step={1} value={r.count}
                    aria-label={`Pool entry ${i + 1} count`}
                    onChange={e => setRow(i, { count: e.target.value })}
                  />
                  <Button type="button" variant="ghost" size="icon" title="Remove this entry" onClick={() => setRows(rows.filter((_, idx) => idx !== i))}>×</Button>
                  <Text variant="hint" className="col-span-4 mb-1">
                    {type ? describeType(type) : ''}{working > 0 && ` ${working} at work.`}
                  </Text>
                </Fragment>
              )
            })}
          </div>
        )}
        <Inline className="justify-between">
          <Button size="sm" onClick={() => setRows([...rows, NEW_ROW])}>+ Add entry</Button>
          <Text as="span" variant="note">Pool wants {total} of max {Number.isInteger(maxN) ? maxN : '?'}.</Text>
        </Inline>
        {maxChanged && !poolChanged && maxN < total && (
          <Text variant="hint">The pool is lowered to fit, from the last entry.</Text>
        )}
      </Stack>

      {problem && <ErrorMessage>{problem}</ErrorMessage>}
      {error && <ErrorMessage>{error}</ErrorMessage>}
      <Inline gap="xs" className="justify-end">
        <Button onClick={onDone} disabled={busy}>Cancel</Button>
        <Button
          variant="primary"
          loading={busy}
          disabled={!!problem || (!maxChanged && !poolChanged)}
          onClick={() => cut.length > 0 ? setConfirming(true) : void save()}
        >Save</Button>
      </Inline>
      {confirming && <CutConfirm max={maxN} cut={cut} nameOf={id => typeName(id, catalog)} onConfirm={() => void save()} onCancel={() => setConfirming(false)} />}
    </Stack>
  )
}

/** Who an experiment keeps at work: its max researchers and its pool, editable in place by its owner. */
export function ResearcherPool({ pe }: { pe: PlatformExperiment }) {
  const { org } = useTenant()
  const readOnly = useReadOnly()
  const { data: roster, error: rosterError } = useRoster(pe)
  const { data: stages, error: stagesError } = useSWR(['pe-stages', pe.id], () => fetchStages(pe.id), { refreshInterval: 15_000 })
  const { data: catalog } = useAgentTypes(org.id)
  const [editing, setEditing] = useState(false)
  const editable = !readOnly && (pe.status === PlatformExperimentStatus.OPEN || pe.status === PlatformExperimentStatus.RUNNING)

  const error = rosterError ?? stagesError
  const atWork = roster && stages ? researchersAtWork(roster, new Set(stages.cut_agents.map(c => c.agent_id))) : undefined
  const working = atWork ? atWorkByKey(atWork) : new Map<string, number>()
  const pool = pe.researchers ?? []
  const inPool = new Set(pool.map(e => poolKey(e.agent_type, e.model)))
  const beyond = atWork?.filter(a => !inPool.has(poolKey(a.flavor, a.model))) ?? []

  return (
    <Section
      title="Researchers"
      note={atWork && `${atWork.length} at work of ${pe.max_agents} max`}
      actions={editable && !editing && atWork && <Button size="sm" onClick={() => setEditing(true)}>Edit</Button>}
    >
      {error && <ErrorMessage>Could not load the researchers: {error.message}</ErrorMessage>}
      {!error && !atWork && <Loading rows={2} />}
      {atWork && editing && <PoolEditor pe={pe} atWork={atWork} onDone={() => setEditing(false)} />}
      {atWork && !editing && (
        <Stack>
          {pool.length === 0 && beyond.length === 0 ? (
            <Text variant="note">No pool. The coordinator adds researchers up to the max of {pe.max_agents}.</Text>
          ) : (
            <Table aria-label="Pool">
              <thead>
                <tr><th>Type</th><th>Model</th><th className="text-right">Wanted</th><th className="text-right">At work</th></tr>
              </thead>
              <tbody>
                {pool.map(e => (
                  <tr key={poolKey(e.agent_type, e.model)}>
                    <td className="text-xs font-medium">{typeName(e.agent_type || DEFAULT_AGENT_TYPE, catalog)}</td>
                    <td><Code className="text-xs">{e.model || 'platform default'}</Code></td>
                    <td className={num}>{e.count}</td>
                    <td className={num}>{working.get(poolKey(e.agent_type, e.model)) ?? 0}</td>
                  </tr>
                ))}
                {beyond.length > 0 && (
                  <tr>
                    <td colSpan={3} className="text-xs text-muted">Added by the coordinator beyond the pool</td>
                    <td className={num}>{beyond.length}</td>
                  </tr>
                )}
              </tbody>
            </Table>
          )}
          {pe.coordinator && pool.length > 0 && (
            <Text variant="hint">The platform keeps these at work and replaces one that finishes or loses its machine while time and budget allow.</Text>
          )}
        </Stack>
      )}
    </Section>
  )
}
