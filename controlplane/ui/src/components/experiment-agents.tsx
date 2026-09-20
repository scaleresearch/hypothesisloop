'use client'

import { useState } from 'react'
import useSWR from 'swr'
import { addPlatformExperimentAgents, fetchPlatformExperimentAgents, removePlatformExperimentAgent } from '@/lib/api'
import { PlatformExperimentStatus } from '@/types'
import type { AgentSignup, JoinedAgent, PlatformExperiment } from '@/types'
import { Button, Chip } from '@/components/ui/button'
import { Pod, PodHeader, PodContent } from '@/components/ui/pod'
import { SecretOnce } from '@/components/ui/secret-once'
import { Loading, ErrorMessage, EmptyState } from '@/components/ui/status-message'

// The specializations the agent harness ships: see agents/coordinator/setup.md. Another name is
// accepted when the harness image carries a prompt for it.
const FLAVORS = ['generalist', 'hyperparameter-search', 'architecture-search', 'data-search']

// What the operator's own `podman run --env-file` takes for one agent.
function envFile(agent: JoinedAgent): string {
  return [
    `AGENT_ID=${agent.agent_id}`,
    `HL_TOKEN=${agent.token}`,
    `PLATFORM_EXPERIMENT_ID=${agent.platform_experiment_id}`,
    ...(agent.flavor ? [`AGENT_FLAVOR=${agent.flavor}`] : []),
    ...(agent.model ? [`ANTHROPIC_MODEL=${agent.model}`] : []),
  ].join('\n')
}

function AddAgents({ pe, onAdded }: { pe: PlatformExperiment; onAdded: () => Promise<unknown> }) {
  const [count, setCount] = useState('1')
  const [hosted, setHosted] = useState(true)
  const [flavor, setFlavor] = useState('')
  const [model, setModel] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  // Agents the operator runs, each with the token this answer alone carries.
  const [selfRun, setSelfRun] = useState<JoinedAgent[]>([])

  const n = Number(count)
  const countValid = Number.isInteger(n) && n >= 1 && n <= 100

  async function submit(e: React.FormEvent) {
    e.preventDefault()
    setBusy(true)
    setError('')
    try {
      const joined = await addPlatformExperimentAgents(pe.id, {
        count: n, hosted, flavor: flavor.trim() || undefined, model: model.trim() || undefined,
      })
      setSelfRun(current => [...current, ...joined.filter(a => a.token)])
      await onAdded()
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div style={{ display: 'grid', gap: 10 }}>
      <form className="settings-row" style={{ alignItems: 'flex-end' }} onSubmit={submit}>
        <label style={{ display: 'grid', gap: 2 }}>
          <span className="uppercase-label">Count</span>
          <input className="field-sm" style={{ width: 70 }} inputMode="numeric" value={count} onChange={e => setCount(e.target.value)} />
        </label>
        <label style={{ display: 'grid', gap: 2 }}>
          <span className="uppercase-label">Flavor</span>
          <input
            className="field-sm mono"
            style={{ width: 200 }}
            list="agent-flavors"
            placeholder="agent's default"
            value={flavor}
            onChange={e => setFlavor(e.target.value)}
          />
          <datalist id="agent-flavors">{FLAVORS.map(f => <option key={f} value={f} />)}</datalist>
        </label>
        <label style={{ display: 'grid', gap: 2 }}>
          <span className="uppercase-label">Model</span>
          <input
            className="field-sm mono"
            style={{ width: 220 }}
            placeholder="agent's default"
            value={model}
            onChange={e => setModel(e.target.value)}
          />
        </label>
        <div style={{ display: 'grid', gap: 2 }}>
          <span className="uppercase-label">Run by</span>
          <div style={{ display: 'flex', gap: 6 }}>
            <Chip type="button" active={hosted} onClick={() => setHosted(true)}>The platform</Chip>
            <Chip type="button" active={!hosted} onClick={() => setHosted(false)}>Me</Chip>
          </div>
        </div>
        <Button size="sm" variant="primary" type="submit" disabled={busy || !countValid}>
          {busy ? 'Adding…' : n === 1 ? 'Add agent' : `Add ${countValid ? n : ''} agents`}
        </Button>
      </form>
      <span className="text-dim" style={{ fontSize: 11 }}>
        {hosted
          ? 'The platform starts one machine per agent and keeps it until the experiment closes or the agent is cut.'
          : 'Each agent comes back with a token. You start the agent yourself with it.'}
        {!countValid && ' Count must be 1 to 100.'}
      </span>
      {error && <ErrorMessage>{error}</ErrorMessage>}
      {selfRun.map(agent => (
        <SecretOnce
          key={agent.agent_id}
          label={`Env file of ${agent.name}`}
          value={envFile(agent)}
          note={<>
            Save it as an env file readable by you alone and start the agent with it. The token acts
            as this agent in this platform experiment and nowhere else. This page shows it now and
            never again.
          </>}
          onDismiss={() => setSelfRun(current => current.filter(a => a.agent_id !== agent.agent_id))}
        />
      ))}
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
  const [removeError, setRemoveError] = useState('')
  // Quota is divided at the start over the agents present, so the roster is fixed from then on.
  const started = pe.status === PlatformExperimentStatus.RUNNING || pe.status === PlatformExperimentStatus.CLOSED

  async function remove(agent: AgentSignup) {
    if (!confirm(`Remove ${agent.name} from this platform experiment? Its token stops working${agent.hosted ? ' and its machine is taken down' : ''}.`)) return
    setRemoveError('')
    try {
      await removePlatformExperimentAgent(pe.id, agent.agent_id)
      await mutate()
    } catch (e) {
      setRemoveError(e instanceof Error ? e.message : String(e))
    }
  }

  return (
    <Pod>
      <PodHeader>Agents{agents ? ` (${agents.length})` : ''}</PodHeader>
      <PodContent scrollX>
        <div style={{ display: 'grid', gap: 14 }}>
          {started
            ? <span className="text-dim" style={{ fontSize: 12 }}>The experiment is {pe.status}: from its start an agent leaves only by being cut or by the experiment closing.</span>
            : <AddAgents pe={pe} onAdded={() => mutate()} />}
          {error && <ErrorMessage>Could not load agents: {error.message}</ErrorMessage>}
          {removeError && <ErrorMessage>{removeError}</ErrorMessage>}
          {!agents && !error && <Loading />}
          {agents && agents.length === 0 && (
            <EmptyState>No agents yet. Nothing runs in this experiment until at least one joins.</EmptyState>
          )}
          {agents && agents.length > 0 && (
            <table className="wa-table">
              <thead>
                <tr><th>Agent</th><th>Role</th><th>Run by</th><th>Flavor</th><th>Model</th><th>Joined</th><th /></tr>
              </thead>
              <tbody>
                {agents.map(agent => (
                  <tr key={agent.agent_id}>
                    <td className="mono" style={{ fontWeight: 600 }}>
                      {agent.name}
                      {agent.name !== agent.agent_id && <div className="text-muted" style={{ fontSize: 11 }}>{agent.agent_id}</div>}
                    </td>
                    <td style={{ fontSize: 12 }}>{agent.role}</td>
                    <td style={{ fontSize: 12 }}>{agent.hosted ? 'the platform' : 'its operator'}</td>
                    <td className="mono" style={{ fontSize: 12 }}>{agent.flavor ?? 'default'}</td>
                    <td className="mono" style={{ fontSize: 12 }}>{agent.model ?? 'default'}</td>
                    <td className="mono text-muted" style={{ fontSize: 12 }}>{new Date(agent.signed_up_at).toLocaleString()}</td>
                    <td>{!started && <Button size="sm" variant="danger" onClick={() => remove(agent)}>Remove</Button>}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
      </PodContent>
    </Pod>
  )
}
