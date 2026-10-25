'use client'

import { useState } from 'react'
import { useSWRConfig } from 'swr'
import { updatePlatformExperiment } from '@/lib/api/platform-experiments'
import type { PlatformExperimentPatch } from '@/types/api'
import { DEFAULT_AGENT_TYPE, RESEARCHER_AGENT_TYPE, typeName, useAgentRoles, useAgentTypes } from '@/lib/agent-types'
import { inheritedModel, useModels } from '@/lib/models'
import { relTime } from '@/lib/format'
import { phaseOf } from '@/lib/phase'
import { atWorkByKey, cutByMax, executorCount, executorsAtWork, poolKey, toTeam, toTeamRows } from '@/lib/team'
import type { TeamRow } from '@/lib/team'
import { useRoster } from '@/lib/roster'
import { useTenant } from '@/lib/tenant'
import { ModelName } from '@/components/model-name'
import { TeamEditor } from '@/components/team-editor'
import { Button } from '@/components/ui/button'
import { Dialog } from '@/components/ui/dialog'
import { Section } from '@/components/ui/section'
import { Inline, Stack } from '@/components/ui/stack'
import { Callout, ErrorMessage, Loading } from '@/components/ui/status-message'
import { Table, num } from '@/components/ui/table'
import { InfoTip } from '@/components/ui/info-tip'
import { Pill, Text } from '@/components/ui/text'
import { refusalMessage } from '@/lib/errors'
import { keys } from '@/lib/keys'
import type { JoinedAgent, PlatformExperiment } from '@/types/api'
import { canEdit } from '@/lib/editable'

const KEEPS_AT_WORK = 'The platform keeps these at work and replaces one that finishes or loses its machine while time and budget allow, except one that ended within 10 minutes or an executor that ended before submitting a job, and at most 3 an hour per entry.'

function plural(n: number, one: string, many = `${one}s`): string {
  return `${n} ${n === 1 ? one : many}`
}

function CutConfirm({ cut, nameOf, onConfirm, onCancel }: {
  cut: JoinedAgent[]
  nameOf: (typeID: string) => string
  onConfirm: () => void
  onCancel: () => void
}) {
  return (
    <Dialog open onClose={onCancel} title={`Cut ${plural(cut.length, 'executor')} at work?`} size="sm">
      <Stack>
        <Text variant="note">
          The team has fewer executors than are at work, so saving cuts the {cut.length === 1 ? 'newest' : `${cut.length} newest`}.
          {cut.length === 1 ? ' It winds down' : ' Each winds down'} as a cut executor does, and nobody takes {cut.length === 1 ? 'its' : 'their'} place.
        </Text>
        <ul className="grid gap-1 rounded border bg-surface-inset px-3 py-2 text-xs" aria-label="Executors to cut">
          {cut.map(a => (
            <li key={a.agent_id} className="flex items-baseline justify-between gap-3">
              <span className="whitespace-nowrap font-medium">{a.name}</span>
              <span className="text-right text-muted">{a.agent_type ? nameOf(a.agent_type) : 'default'} · joined {relTime(a.signed_up_at)}</span>
            </li>
          ))}
        </ul>
        <Inline gap="xs" className="justify-end">
          <Button onClick={onCancel}>Cancel</Button>
          <Button variant="danger" onClick={onConfirm}>Save and cut {cut.length}</Button>
        </Inline>
      </Stack>
    </Dialog>
  )
}

function TeamChange({ pe, atWork, onDone }: { pe: PlatformExperiment; atWork: JoinedAgent[]; onDone: () => void }) {
  const { org } = useTenant()
  const { mutate } = useSWRConfig()
  const { data: catalog } = useAgentTypes(org.id)
  const [rows, setRows] = useState<TeamRow[]>(() => toTeamRows(pe.agents))
  const [confirming, setConfirming] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  const opened = JSON.stringify(toTeamRows(pe.agents))
  const teamChanged = JSON.stringify(rows) !== opened
  // The API holds the team's executors as the most at work at once.
  const total = executorCount(rows)
  const maxN = teamChanged && total > 0 ? total : pe.max_agents
  const running = pe.status === 'running'
  const executors = executorsAtWork(atWork)
  const cut = running ? cutByMax(executors, maxN) : []
  const remaining = atWork.filter(a => !cut.includes(a))

  async function save() {
    setConfirming(false)
    const team = toTeam(rows)
    if ('problem' in team) return setError(team.problem)
    const patch: PlatformExperimentPatch = { agents: team.agents }
    setBusy(true)
    setError('')
    try {
      const saved = await updatePlatformExperiment(pe.id, patch)
      await mutate(keys.experiment(pe.id), saved, { revalidate: false })
      await mutate(keys.roster(pe.id))
      onDone()
    } catch (e) {
      setError(refusalMessage(e, patch))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Stack gap="lg">
      <TeamEditor
        rows={rows}
        onChange={setRows}
        coordinatorModel={pe.coordinator?.model ?? ''}
        coordinator={pe.coordinator ? {} : false}
        atWork={row => atWorkByKey(remaining).get(poolKey(row.agent_type, row.model.trim())) ?? 0}
      />
      {cut.length > 0 && <Text variant="hint">Saving cuts the {plural(cut.length, 'newest executor')} at work.</Text>}
      {error && <ErrorMessage>{error}</ErrorMessage>}
      <Inline gap="xs" className="justify-end">
        <Button onClick={onDone} disabled={busy}>Cancel</Button>
        <Button
          variant="primary"
          loading={busy}
          disabled={!teamChanged}
          onClick={() => cut.length > 0 ? setConfirming(true) : void save()}
        >Save</Button>
      </Inline>
      {confirming && <CutConfirm cut={cut} nameOf={id => typeName(id, catalog)} onConfirm={() => void save()} onCancel={() => setConfirming(false)} />}
    </Stack>
  )
}

/** Who an experiment keeps at work: its team, editable in place by its owner, whose executors are its max at work. */
export function Team({ pe }: { pe: PlatformExperiment }) {
  const { org } = useTenant()
  const { data: roster, error } = useRoster(pe)
  const { data: catalog } = useAgentTypes(org.id)
  const { data: roles } = useAgentRoles()
  const { data: models } = useModels()
  const [editing, setEditing] = useState(false)
  const editable = canEdit(pe, 'agents')
  const closed = phaseOf(pe) === 'closed'

  const atWork = roster?.filter(a => a.at_work)
  const counts = pe.agents_at_work
  const team = pe.agents ?? []
  const beyond = counts?.outside_team ?? 0
  const roleName = (agentType: string) => {
    const role = catalog?.find(t => t.id === agentType)?.role
    return roles?.find(r => r.role === role)?.name
  }
  // What an entry that names no model runs on, as the platform resolves it.
  const runsOn = (agentType: string) => {
    const { inherited, inheritedFrom } = inheritedModel(catalog?.find(t => t.id === agentType)?.current.settings.model, agentType === RESEARCHER_AGENT_TYPE, pe.coordinator?.model || models?.coordinator_model)
    if (inherited && inheritedFrom) return { model: inherited, from: inheritedFrom.toLowerCase() }
    return models && { model: models.default_model, from: 'platform default' }
  }

  return (
    <Section
      title="Team"
      note={counts && (closed ? plural(pe.agents?.length ?? 0, 'entry', 'entries') : `${counts.total} ${counts.total === 1 ? 'executor' : 'executors'} at work of ${pe.max_agents} max`)}
      actions={editable && !editing && atWork && <Button size="sm" onClick={() => setEditing(true)}>Edit</Button>}
    >
      {error && <ErrorMessage>Could not load the team: {error.message}</ErrorMessage>}
      {!counts && <Loading rows={2} />}
      {atWork && editing && <TeamChange pe={pe} atWork={atWork} onDone={() => setEditing(false)} />}
      {counts && !editing && (
        <Stack>
          {team.length === 0 && beyond === 0 ? (
            <Text variant="note">No team. The coordinator adds executors up to the max of {pe.max_agents}, and researchers.</Text>
          ) : (
            <Table aria-label="Team">
              <thead>
                <tr>
                  <th>Type</th>
                  <th>Model</th>
                  <th className="w-24 text-right">
                    Wanted
                    {!closed && pe.coordinator && team.length > 0 && <InfoTip>{KEEPS_AT_WORK}</InfoTip>}
                  </th>
                  {!closed && <th className="w-24 text-right">At work</th>}
                </tr>
              </thead>
              <tbody>
                {team.map((e, i) => {
                  const id = e.agent_type || DEFAULT_AGENT_TYPE
                  const role = roleName(id)
                  const resolved = e.model ? undefined : runsOn(id)
                  return (
                    <tr key={poolKey(e.agent_type, e.model)}>
                      <td className="text-xs font-medium">
                        <Inline gap="xs">{typeName(id, catalog)}{role && <Pill tone="accent">{role}</Pill>}</Inline>
                      </td>
                      <td className="text-xs">
                        {e.model ? <ModelName model={e.model} /> : resolved ? (
                          <div className="flex items-baseline gap-2"><ModelName model={resolved.model} className="flex-none" /><span className="min-w-0 truncate text-2xs text-muted">({resolved.from})</span></div>
                        ) : <span className="text-muted">inherited</span>}
                      </td>
                      <td className={num}>{e.count}</td>
                      {!closed && <td className={num}>{counts.pool[i]?.at_work ?? 0}</td>}
                    </tr>
                  )
                })}
                {beyond > 0 && !closed && (
                  <tr>
                    <td colSpan={3} className="text-xs text-muted">Added by the coordinator beyond the team</td>
                    <td className={num}>{beyond}</td>
                  </tr>
                )}
              </tbody>
            </Table>
          )}
          {pe.agents_held && <Callout tone="warning">{pe.agents_held}</Callout>}
        </Stack>
      )}
    </Section>
  )
}
