'use client'

import { useState } from 'react'
import useSWR, { useSWRConfig } from 'swr'
import {
  addPlatformExperimentAgents, apiURL, fetchPlatformExperimentAgents, fetchPlatformExperimentAgentToken, removePlatformExperimentAgent,
  startPlatformExperiment,
} from '@/lib/api'
import { PlatformExperimentStatus } from '@/types'
import type { AgentSignup, JoinedAgent, PlatformExperiment } from '@/types'
import { Button, Chip } from '@/components/ui/button'
import { Card, CardHeader, CardContent } from '@/components/ui/card'
import { ConfirmDialog } from '@/components/ui/dialog'
import { FormField } from '@/components/ui/form-field'
import { Input } from '@/components/ui/input'
import { Table } from '@/components/ui/table'
import { Eyebrow } from '@/components/ui/text'
import { Secret } from '@/components/ui/secret'
import { InstanceHealth } from '@/components/instance-health'
import { clearRunFailure, runFailure } from '@/lib/run-experiment'
import { Loading, ErrorMessage, EmptyState } from '@/components/ui/status-message'
import { useReadOnly } from '@/lib/read-only'

// The specializations the agent harness ships: see agents/coordinator/setup.md. Another name is
// accepted when the harness image carries a prompt for it.
const FLAVORS = ['generalist', 'hyperparameter-search', 'architecture-search', 'data-search']

// What the operator's own `podman run --env-file` takes for one agent, and all an agent is
// started with: its token says which agent of which platform experiment it is, and it reads its
// flavor and model from its signup.
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
    <div className="grid gap-2.5">
      <form className="flex flex-wrap items-end gap-2" onSubmit={submit}>
        <FormField label="Flavor">
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
        <div className="grid gap-1" role="group" aria-label="Run by">
          <Eyebrow>Run by</Eyebrow>
          <div className="flex gap-1.5">
            <Chip type="button" active={hosted} onClick={() => setHosted(true)}>The platform</Chip>
            <Chip type="button" active={!hosted} onClick={() => setHosted(false)}>Me</Chip>
          </div>
        </div>
        <Button size="sm" variant="primary" type="submit" disabled={busy}>
          {busy ? 'Adding…' : 'Add agent'}
        </Button>
      </form>
      <span className="text-xs text-subtle">
        {hosted
          ? 'The platform starts a machine for the agent and keeps it until the experiment closes or the agent is cut.'
          : 'The agent comes back with a token. You start the agent yourself with it, and can show the token again from the agent\'s row.'}
      </span>
      {error && <ErrorMessage>{error}</ErrorMessage>}
    </div>
  )
}

/** Who takes part in a platform experiment, and while it has not started, adding and removing them. */
export function ExperimentAgents({ pe }: { pe: PlatformExperiment }) {
  const { data: agents, error, mutate } = useSWR(
    ['pe-agents', pe.id],
    () => fetchPlatformExperimentAgents(pe.id),
    { refreshInterval: 15_000 },
  )
  const [rowError, setRowError] = useState('')
  // Self-run agents whose token is on screen: the ones just added, and any asked for again.
  const [shown, setShown] = useState<JoinedAgent[]>([])
  const [removing, setRemoving] = useState<AgentSignup | null>(null)
  const [starting, setStarting] = useState<'asked' | 'sent' | null>(null)
  // Set when the create form went on to run this experiment and the API stopped it part way.
  const [stopped, setStopped] = useState(() => runFailure(pe.id))
  function forgetStopped() {
    clearRunFailure(pe.id)
    setStopped(undefined)
  }
  const { mutate: reread } = useSWRConfig()
  const readOnly = useReadOnly()
  // Quota is divided at the start over the agents present, so the roster is fixed from then on.
  const started = pe.status === PlatformExperimentStatus.RUNNING || pe.status === PlatformExperimentStatus.CLOSED

  async function start() {
    setStarting('sent')
    setRowError('')
    try {
      await startPlatformExperiment(pe.id)
      forgetStopped()
      // The page reads the experiment under this key, and its status is what just changed.
      await reread(['pe', pe.id])
    } catch (e) {
      setRowError(e instanceof Error ? e.message : String(e))
    } finally {
      setStarting(null)
    }
  }

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
    <Card>
      <CardHeader>Agents{agents ? ` (${agents.length})` : ''}</CardHeader>
      <CardContent scrollX>
        <div className="grid gap-3.5">
          {stopped && (
            <ErrorMessage>
              This platform experiment was created, but it is not running as asked: {stopped.step} failed. {stopped.message}{' '}
              Nothing was undone. Finish with the controls below.{' '}
              <Button variant="link" size="sm" onClick={forgetStopped}>Dismiss</Button>
            </ErrorMessage>
          )}
          {readOnly ? null : started
            ? <span className="text-xs text-subtle">The experiment is {pe.status}: from its start an agent leaves only by being cut or by the experiment closing.</span>
            : <AddAgents pe={pe} onAdded={joined => { show(joined); return mutate() }} />}
          {!readOnly && pe.status === PlatformExperimentStatus.OPEN && (
            <div className="flex flex-wrap items-center gap-2.5">
              <Button size="sm" variant="primary" loading={starting === 'sent'} onClick={() => setStarting('asked')}>Start now</Button>
              <span className="text-xs text-subtle">Otherwise it starts by itself at its start time. Either way the agents present then are the ones that take part.</span>
            </div>
          )}
          {shown.map(agent => (
            <Secret
              key={agent.agent_id}
              label={`Env file of ${agent.name}`}
              value={envFile(agent)}
              note={<>
                Save it as an env file readable by you alone and start the agent with it. The token
                acts as this agent in this platform experiment and nowhere else. It is derived from
                the agent&apos;s place here, so <span className="font-mono">Show token</span> in the
                agent&apos;s row brings the same one back, as does{' '}
                <span className="font-mono">hl platform-experiments agents add {pe.id} --agent {agent.agent_id}</span>.
                Removing the agent is what stops it working.
              </>}
              dismiss="Hide"
              onDismiss={() => hide(agent.agent_id)}
            />
          ))}
          {pe.coordinator && (
            <div role="group" aria-label="Coordinator" className="grid gap-1.5 rounded border bg-surface-inset px-3 py-2.5">
              <div className="flex flex-wrap items-baseline gap-x-2.5">
                <Eyebrow>Coordinator</Eyebrow>
                <span className="text-xs text-muted">run by the platform · model <span className="font-mono">{pe.coordinator.model}</span></span>
              </div>
              <InstanceHealth instance={pe.coordinator.instance} />
            </div>
          )}
          {error && <ErrorMessage>Could not load agents: {error.message}</ErrorMessage>}
          {rowError && <ErrorMessage>{rowError}</ErrorMessage>}
          {!agents && !error && <Loading />}
          {agents && agents.length === 0 && (
            <EmptyState>No agents yet.{!readOnly && ' Nothing runs in this experiment until at least one joins.'}</EmptyState>
          )}
          {agents && agents.length > 0 && (
            <Table>
              <thead>
                <tr><th>Agent</th><th>Role</th><th>Run by</th><th>Machine</th><th>Flavor</th><th>Model</th><th>Joined</th><th /></tr>
              </thead>
              <tbody>
                {agents.map(agent => (
                  <tr key={agent.agent_id}>
                    <td className="whitespace-nowrap font-mono text-xs font-semibold">
                      {agent.name}
                      {agent.name !== agent.agent_id && <div className="text-2xs font-normal text-muted">{agent.agent_id}</div>}
                    </td>
                    <td className="text-xs">{agent.role}</td>
                    <td className="whitespace-nowrap text-xs">{agent.hosted ? 'the platform' : 'its operator'}</td>
                    <td>
                      {agent.instance
                        ? <InstanceHealth instance={agent.instance} />
                        : <span className="text-xs text-subtle">{agent.hosted ? 'not reported yet' : 'not the platform\'s'}</span>}
                    </td>
                    <td className="whitespace-nowrap font-mono text-xs">{agent.flavor ?? 'default'}</td>
                    <td className="whitespace-nowrap font-mono text-xs">{agent.model ?? 'default'}</td>
                    <td className="font-mono text-xs text-muted">{new Date(agent.signed_up_at).toLocaleString()}</td>
                    <td>
                      {!readOnly && (
                        <div className="flex justify-end gap-1.5">
                          {!agent.hosted && <Button size="sm" onClick={() => showToken(agent)}>Show token</Button>}
                          {!started && <Button size="sm" variant="danger" onClick={() => setRemoving(agent)}>Remove</Button>}
                        </div>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </Table>
          )}
        </div>
        <ConfirmDialog
          open={starting === 'asked'}
          title="Start this platform experiment now?"
          confirmLabel="Start"
          variant="primary"
          onConfirm={start}
          onCancel={() => setStarting(null)}
        >
          Signup closes and the budget is divided among the agents that have joined. No agent can join or be removed afterwards.
        </ConfirmDialog>
        {removing && (
          <ConfirmDialog
            open
            title={`Remove ${removing.name} from this platform experiment?`}
            confirmLabel="Remove"
            onConfirm={() => remove(removing)}
            onCancel={() => setRemoving(null)}
          >
            Its token stops working{removing.hosted ? ' and its machine is taken down' : ''}.
          </ConfirmDialog>
        )}
      </CardContent>
    </Card>
  )
}
