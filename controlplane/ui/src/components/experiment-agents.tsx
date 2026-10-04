'use client'

import { Fragment, useState } from 'react'
import { clsx } from 'clsx'
import { ChevronRight } from 'lucide-react'
import { addPlatformExperimentAgents, apiURL, fetchPlatformExperimentAgentToken, removePlatformExperimentAgent } from '@/lib/api'
import { useRoster } from '@/lib/roster'
import { PlatformExperimentStatus } from '@/types'
import type { AgentSignup, JoinedAgent, PlatformExperiment } from '@/types'
import { StatusLabel } from '@/components/ui/badge'
import { Button, Chip } from '@/components/ui/button'
import { InsetCard } from '@/components/ui/card'
import { ConfirmDialog } from '@/components/ui/dialog'
import { FormField } from '@/components/ui/form-field'
import { Input } from '@/components/ui/input'
import { Table } from '@/components/ui/table'
import { EmptyState } from '@/components/ui/empty-state'
import { Facts } from '@/components/ui/facts'
import { Section } from '@/components/ui/section'
import { Inline, Stack } from '@/components/ui/stack'
import { Code, Eyebrow, Text } from '@/components/ui/text'
import { Secret } from '@/components/ui/secret'
import { Activity } from '@/components/agent-at-work'
import { InstanceHealth } from '@/components/instance-health'
import { clearRunFailure, runFailure } from '@/lib/run-experiment'
import { relTime } from '@/lib/format'
import { Loading, ErrorMessage } from '@/components/ui/status-message'
import { useReadOnly } from '@/lib/read-only'
import type { Tone } from '@/lib/colors'

// The specializations the agent harness ships: see agents/coordinator/setup.md. Another name is
// accepted when the harness image carries a prompt for it.
const FLAVORS = ['generalist', 'hyperparameter-search', 'architecture-search', 'data-search']

// What the operator's own `podman run --env-file` takes for one agent, and all an agent is
// started with: its token says which agent of which platform experiment it is, and it reads its
// specialization and model from its signup.
function envFile(agent: JoinedAgent): string {
  return [`API_URL=${apiURL()}`, `HL_TOKEN=${agent.token}`].join('\n')
}

function AddAgents({ pe, onAdded }: { pe: PlatformExperiment; onAdded: (joined: JoinedAgent[]) => Promise<unknown> }) {
  const [hosted, setHosted] = useState(true)
  const [flavor, setFlavor] = useState('')
  const [model, setModel] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  async function submit(e: React.FormEvent) {
    e.preventDefault()
    setBusy(true)
    setError('')
    try {
      await onAdded(await addPlatformExperimentAgents(pe.id, {
        hosted, flavor: flavor.trim() || undefined, model: model.trim() || undefined,
      }))
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Stack>
      <Inline as="form" align="end" onSubmit={submit}>
        <FormField label="Specialization">
          <Input
            fieldSize="sm"
            mono
            className="w-52"
            list="agent-flavors"
            placeholder="agent's default"
            value={flavor}
            onChange={e => setFlavor(e.target.value)}
          />
          <datalist id="agent-flavors">{FLAVORS.map(f => <option key={f} value={f} />)}</datalist>
        </FormField>
        <FormField label="Model">
          <Input
            fieldSize="sm"
            mono
            className="w-56"
            placeholder="agent's default"
            value={model}
            onChange={e => setModel(e.target.value)}
          />
        </FormField>
        <Stack gap="xs" role="group" aria-label="Run by">
          <Eyebrow>Run by</Eyebrow>
          <Inline gap="xs" nowrap>
            <Chip type="button" active={hosted} onClick={() => setHosted(true)}>The platform</Chip>
            <Chip type="button" active={!hosted} onClick={() => setHosted(false)}>Me</Chip>
          </Inline>
        </Stack>
        <Button size="sm" variant="primary" type="submit" loading={busy}>Add agent</Button>
      </Inline>
      <Text variant="hint">
        {hosted
          ? 'The platform starts a machine for the agent and keeps it until the experiment closes or the agent finishes or is cut.'
          : 'The agent comes back with a token. You start the agent yourself with it, and can show the token again from the agent\'s row.'}
      </Text>
      {error && <ErrorMessage>{error}</ErrorMessage>}
    </Stack>
  )
}

// One short word on where an agent is, and the longer story behind it for its title.
function statusOf(agent: JoinedAgent, closed: boolean): { label: string; tone: Tone; detail?: string } {
  const detail = [agent.finished_reason, agent.activity?.text, agent.status_reason?.message].filter(Boolean).join(' ') || undefined
  if (agent.finished_at) return { label: `Finished ${relTime(agent.finished_at)}`, tone: 'neutral', detail }
  if (closed) return { label: 'Stopped, experiment closed', tone: 'neutral', detail }
  if (!agent.instance) return { label: agent.hosted ? 'Not reported yet' : 'Run by its operator', tone: 'neutral', detail }
  switch (agent.instance.state) {
    case 'running': return { label: 'Running', tone: 'success', detail }
    case 'starting': return { label: 'Starting', tone: 'warning', detail }
    case 'requested': return { label: 'Machine asked for', tone: 'neutral', detail }
    case 'waiting': return { label: 'Waiting for the start', tone: 'neutral', detail }
    case 'stopped': return agent.instance.end_reason === 'released'
      ? { label: 'Stopped, run over', tone: 'neutral', detail }
      : { label: 'Stopped', tone: 'danger', detail }
  }
}

/** Everything about one agent that its row has no room for. */
function AgentDetails({ agent }: { agent: JoinedAgent }) {
  return (
    <Stack gap="sm">
      <Facts facts={[
        ['Id', agent.agent_id],
        ['Run by', agent.hosted ? 'the platform' : 'its operator'],
        ['Specialization', agent.flavor ?? 'default'],
        ['Model', agent.model ?? 'default'],
        ['Joined', new Date(agent.signed_up_at).toLocaleString()],
      ]} />
      {agent.finished_at && (
        <Text variant="note" title={new Date(agent.finished_at).toLocaleString()}>
          Finished {relTime(agent.finished_at)}: {agent.finished_reason}
        </Text>
      )}
      {agent.instance && <InstanceHealth instance={agent.instance} />}
      {agent.activity && <Activity activity={agent.activity} className="max-w-md" />}
      {agent.status_reason && agent.instance && (
        <Text variant="hint" className="max-w-md">{agent.status_reason.message}</Text>
      )}
    </Stack>
  )
}

/** The anchor of the agents card, for a link that says to add a researcher. */
export const AGENTS_CARD = 'agents'

/** Who takes part in a platform experiment, and while it has not started, adding and removing them. */
export function ExperimentAgents({ pe }: { pe: PlatformExperiment }) {
  const { data: agents, error, mutate } = useRoster(pe)
  const [rowError, setRowError] = useState('')
  // Self-run agents whose token is on screen: the ones just added, and any asked for again.
  const [shown, setShown] = useState<JoinedAgent[]>([])
  const [removing, setRemoving] = useState<AgentSignup | null>(null)
  const [open, setOpen] = useState<string | null>(null)
  // Set when the create form went on to run this experiment and the API stopped it part way.
  const [stopped, setStopped] = useState(() => runFailure(pe.id))
  function forgetStopped() {
    clearRunFailure(pe.id)
    setStopped(undefined)
  }
  const readOnly = useReadOnly()
  // Quota is divided at the start over the agents present, so the roster is fixed from then on.
  const started = pe.status === PlatformExperimentStatus.RUNNING || pe.status === PlatformExperimentStatus.CLOSED

  async function remove(agent: AgentSignup) {
    setRemoving(null)
    setRowError('')
    try {
      await removePlatformExperimentAgent(pe.id, agent.agent_id)
      hide(agent.agent_id)
      await mutate()
    } catch (e) {
      setRowError(e instanceof Error ? e.message : String(e))
    }
  }

  function hide(agentID: string) {
    setShown(current => current.filter(a => a.agent_id !== agentID))
  }

  function show(agents: JoinedAgent[]) {
    const tokens = agents.filter(a => a.token)
    setShown(current => [...current.filter(a => !tokens.some(t => t.agent_id === a.agent_id)), ...tokens])
  }

  async function showToken(agent: AgentSignup) {
    setRowError('')
    try {
      show([await fetchPlatformExperimentAgentToken(pe.id, agent.agent_id)])
    } catch (e) {
      setRowError(e instanceof Error ? e.message : String(e))
    }
  }

  return (
    <Section id={AGENTS_CARD} title="Agents" note={agents && `(${agents.length})`} scrollX>
        <Stack>
          {stopped && (
            <ErrorMessage>
              This experiment was created, but it is not running as asked: {stopped.step} failed. {stopped.message}{' '}
              Nothing was undone. Finish with the controls below.{' '}
              <Button variant="link" size="sm" onClick={forgetStopped}>Dismiss</Button>
            </ErrorMessage>
          )}
          {!readOnly && !started && <AddAgents pe={pe} onAdded={joined => { show(joined); return mutate() }} />}
          {shown.map(agent => (
            <Secret
              key={agent.agent_id}
              label={`Env file of ${agent.name}`}
              value={envFile(agent)}
              note={<>
                Save it as an env file readable by you alone and start the agent with it. The token
                works for this agent in this experiment and nowhere else. It never changes:{' '}
                <span className="font-medium">Show token</span> in the agent&apos;s row shows the same one again, as does{' '}
                <Code>hl platform-experiments agents add --platform-experiment {pe.id} --agent {agent.agent_id}</Code>.
                Removing the agent is what stops it working.
              </>}
              dismiss="Hide"
              onDismiss={() => hide(agent.agent_id)}
            />
          ))}
          {pe.coordinator && (
            <InsetCard role="group" aria-label="Coordinator">
              <Inline align="baseline">
                <Eyebrow>Coordinator</Eyebrow>
                <Text as="span" variant="note">run by the platform · model <Code>{pe.coordinator.model}</Code></Text>
              </Inline>
              <InstanceHealth instance={pe.coordinator.instance} />
              {pe.coordinator.activity && <Activity activity={pe.coordinator.activity} />}
            </InsetCard>
          )}
          {error && <ErrorMessage>Could not load agents: {error.message}</ErrorMessage>}
          {rowError && <ErrorMessage>{rowError}</ErrorMessage>}
          {!agents && !error && <Loading />}
          {agents && agents.length === 0 && (
            <EmptyState>No agents yet.{!readOnly && ' Nothing runs in this experiment until at least one joins.'}</EmptyState>
          )}
          {agents && agents.length > 0 && (
            <Table aria-label="Agents">
              <thead>
                <tr><th className="w-8" /><th>Agent</th><th>Role</th><th>Status</th><th /></tr>
              </thead>
              <tbody>
                {agents.map(agent => {
                  const status = statusOf(agent, pe.status === PlatformExperimentStatus.CLOSED)
                  const expanded = open === agent.agent_id
                  return (
                    <Fragment key={agent.agent_id}>
                      <tr>
                        <td>
                          <Button
                            variant="ghost"
                            size="icon"
                            aria-expanded={expanded}
                            aria-label={`Details of ${agent.name}`}
                            onClick={() => setOpen(expanded ? null : agent.agent_id)}
                          >
                            <ChevronRight aria-hidden="true" size={13} className={clsx('transition-transform', expanded && 'rotate-90')} />
                          </Button>
                        </td>
                        <td className="whitespace-nowrap text-xs font-medium" title={agent.agent_id}>{agent.name}</td>
                        <td className="text-xs">{agent.role}</td>
                        <td><StatusLabel tone={status.tone} title={status.detail}>{status.label}</StatusLabel></td>
                        <td>
                          {!readOnly && (
                            <Inline gap="xs" nowrap className="justify-end">
                              {!agent.hosted && <Button size="sm" onClick={() => showToken(agent)}>Show token</Button>}
                              {!started && <Button size="sm" variant="danger" onClick={() => setRemoving(agent)}>Remove</Button>}
                            </Inline>
                          )}
                        </td>
                      </tr>
                      {expanded && (
                        <tr>
                          <td colSpan={5} className="bg-surface-inset">
                            <AgentDetails agent={agent} />
                          </td>
                        </tr>
                      )}
                    </Fragment>
                  )
                })}
              </tbody>
            </Table>
          )}
        </Stack>
        {removing && (
          <ConfirmDialog
            open
            title={`Remove ${removing.name} from this experiment?`}
            confirmLabel="Remove"
            onConfirm={() => remove(removing)}
            onCancel={() => setRemoving(null)}
          >
            Its token stops working{removing.hosted ? ' and its machine is taken down' : ''}.
          </ConfirmDialog>
        )}
    </Section>
  )
}
