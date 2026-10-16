'use client'

import { Fragment, useState } from 'react'
import useSWR from 'swr'
import { clsx } from 'clsx'
import { ChevronRight, MessageSquare } from 'lucide-react'
import {
  addPlatformExperimentAgents, apiURL, fetchPlatformExperimentAgentToken, fetchPlatformExperimentMessages,
  removePlatformExperimentAgent, sendPlatformExperimentMessage,
} from '@/lib/api'
import { pollEvery, useRoster } from '@/lib/roster'
import { PlatformExperimentStatus } from '@/types'
import type { AgentMessage, AgentSignup, JoinedAgent, PlatformExperiment } from '@/types'
import { StatusLabel } from '@/components/ui/badge'
import { Button, Chip } from '@/components/ui/button'
import { InsetCard } from '@/components/ui/card'
import { ConfirmDialog, Dialog } from '@/components/ui/dialog'
import { FormField } from '@/components/ui/form-field'
import { Input, Textarea } from '@/components/ui/input'
import { Table } from '@/components/ui/table'
import { EmptyState } from '@/components/ui/empty-state'
import { Facts } from '@/components/ui/facts'
import { Section } from '@/components/ui/section'
import { Inline, Stack } from '@/components/ui/stack'
import { Code, Eyebrow, Text } from '@/components/ui/text'
import { Secret } from '@/components/ui/secret'
import { Activity } from '@/components/agent-at-work'
import { InstanceHealth } from '@/components/instance-health'
import { DEFAULT_AGENT_TYPE, researcherTypes, typeName, useAgentTypes } from '@/lib/agent-types'
import { useTenant } from '@/lib/tenant'
import { errorMessage } from '@/lib/errors'
import { Select } from '@/components/ui/select'
import type { AgentType } from '@/types'
import { relTime } from '@/lib/format'
import { Loading, ErrorMessage } from '@/components/ui/status-message'
import { useReadOnly } from '@/lib/read-only'
import type { Tone } from '@/lib/colors'


// What the operator's own `podman run --env-file` takes for one agent, and all an agent is
// started with: its token says which agent of which platform experiment it is, and it reads its
// specialization and model from its signup.
function envFile(agent: JoinedAgent): string {
  return [`API_URL=${apiURL()}`, `HL_TOKEN=${agent.token}`].join('\n')
}

function AddAgents({ pe, onAdded }: { pe: PlatformExperiment; onAdded: (joined: JoinedAgent[]) => Promise<unknown> }) {
  const { org } = useTenant()
  const { data: catalog } = useAgentTypes(org.id)
  const [hosted, setHosted] = useState(true)
  const [flavor, setFlavor] = useState(DEFAULT_AGENT_TYPE)
  const [model, setModel] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  async function submit(e: React.FormEvent) {
    e.preventDefault()
    setBusy(true)
    setError('')
    try {
      await onAdded(await addPlatformExperimentAgents(pe.id, {
        hosted, flavor, model: model.trim() || undefined,
      }))
    } catch (e) {
      setError(errorMessage(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Stack>
      <Inline as="form" align="end" onSubmit={submit}>
        <FormField label="Type">
          <Select
            fieldSize="sm"
            className="w-56"
            value={flavor}
            placeholder={typeName(flavor, catalog)}
            options={researcherTypes(catalog).map(t => ({ value: t.id, label: t.builtin ? t.name : `${t.name} (v${t.current_version})` }))}
            onChange={setFlavor}
          />
        </FormField>
        <FormField label="Model">
          <Input
            fieldSize="sm"
            mono
            className="w-56"
            placeholder={catalog?.find(t => t.id === flavor)?.current.settings.model || 'platform default'}
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
  const detail = [agent.finished_reason, agent.activity?.text, agent.status_reason?.summary].filter(Boolean).join(' ') || undefined
  if (agent.finished_at) return { label: `Finished ${relTime(agent.finished_at)}`, tone: 'neutral', detail }
  if (agent.cut_reason === 'max_agents_lowered') return { label: 'Cut, max researchers lowered', tone: 'neutral', detail }
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

/** Writes a message to one agent, which wakes it at once. */
function MessageAgent({ pe, agent, onSent, onClose }: {
  pe: PlatformExperiment; agent: AgentSignup; onSent: () => Promise<unknown>; onClose: () => void
}) {
  const [text, setText] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  async function send(e: React.FormEvent) {
    e.preventDefault()
    setBusy(true)
    setError('')
    try {
      await sendPlatformExperimentMessage(pe.id, agent.agent_id, text.trim())
      await onSent()
      onClose()
    } catch (e) {
      setError(errorMessage(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Dialog open onClose={onClose} title={`Message ${agent.name}`} size="sm">
      <Stack as="form" onSubmit={send}>
        <Textarea aria-label="Message" rows={4} value={text} onChange={e => setText(e.target.value)} autoFocus />
        <Text variant="hint">It wakes the agent at once, as a message from the owner.</Text>
        {error && <ErrorMessage>{error}</ErrorMessage>}
        <Inline gap="xs" className="justify-end">
          <Button type="button" onClick={onClose}>Cancel</Button>
          <Button type="submit" variant="primary" loading={busy} disabled={!text.trim()}>Send</Button>
        </Inline>
      </Stack>
    </Dialog>
  )
}

/** The messages an agent was sent, oldest first. */
function Messages({ messages }: { messages: AgentMessage[] }) {
  return (
    <Stack gap="xs" aria-label="Messages">
      <Eyebrow>Messages</Eyebrow>
      {messages.map((m, i) => (
        <Text key={i} variant="note" className="max-w-md" title={new Date(m.at).toLocaleString()}>
          From the {m.from}, {relTime(m.at)}: {m.text}
        </Text>
      ))}
    </Stack>
  )
}

/** Everything about one agent that its row has no room for. */
/** The agent type an agent runs, by name. */
function AgentTypeLabel({ agent, catalog }: { agent: AgentSignup; catalog: AgentType[] | undefined }) {
  if (!agent.flavor) return <span className="text-muted">default</span>
  return <span title={agent.flavor}>{typeName(agent.flavor, catalog)}</span>
}

function AgentDetails({ agent, messages, catalog }: { agent: JoinedAgent; messages: AgentMessage[]; catalog: AgentType[] | undefined }) {
  return (
    <Stack gap="sm">
      <Facts facts={[
        ['Id', agent.agent_id],
        ['Run by', agent.hosted ? 'the platform' : 'its operator'],
        ['Type', <AgentTypeLabel key="type" agent={agent} catalog={catalog} />],
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
        <Text variant="hint" className="max-w-md">{agent.status_reason.summary}</Text>
      )}
      {agent.status_reason?.operator_detail && (
        <Text variant="note" className="max-w-md">Operator: {agent.status_reason.operator_detail}</Text>
      )}
      {messages.length > 0 && <Messages messages={messages} />}
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
  const [messaging, setMessaging] = useState<AgentSignup | null>(null)
  const [open, setOpen] = useState<string | null>(null)
  const readOnly = useReadOnly()
  const { org } = useTenant()
  const { data: catalog } = useAgentTypes(org.id)
  // Quota is divided at the start over the agents present, so the roster is fixed from then on.
  const started = pe.status === PlatformExperimentStatus.RUNNING || pe.status === PlatformExperimentStatus.CLOSED
  const { data: messages, mutate: mutateMessages } = useSWR(
    pe.status === PlatformExperimentStatus.DRAFT ? null : ['pe-messages', pe.id],
    () => fetchPlatformExperimentMessages(pe.id),
    { refreshInterval: pollEvery(pe) },
  )
  const at = pe.status === PlatformExperimentStatus.OPEN || pe.status === PlatformExperimentStatus.RUNNING
  const messagesTo = (agent: AgentSignup) => messages?.filter(m => m.agent_id === agent.agent_id) ?? []

  async function remove(agent: AgentSignup) {
    await removePlatformExperimentAgent(pe.id, agent.agent_id)
    hide(agent.agent_id)
    await mutate()
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
      setRowError(errorMessage(e))
    }
  }

  return (
    <Section id={AGENTS_CARD} title="Agents" note={agents && `(${agents.length})`} scrollX>
        <Stack>
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
            <EmptyState>No agents yet.</EmptyState>
          )}
          {agents && agents.length > 0 && (
            <Table aria-label="Agents">
              <thead>
                <tr><th className="w-8" /><th>Agent</th><th>Role</th><th>Type</th><th>Status</th><th /></tr>
              </thead>
              <tbody>
                {agents.map(agent => {
                  const status = statusOf(agent, pe.status === PlatformExperimentStatus.CLOSED)
                  const expanded = open === agent.agent_id
                  const sent = messagesTo(agent)
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
                        <td className="whitespace-nowrap text-xs font-medium" title={agent.agent_id}>
                          {agent.name}
                          {sent.length > 0 && (
                            <Text as="span" variant="hint" className="ml-2 inline-flex items-center gap-0.5 font-normal" title={`Last message: ${sent.at(-1)!.text}`}>
                              <MessageSquare aria-hidden="true" size={11} />{sent.length}
                            </Text>
                          )}
                        </td>
                        <td className="text-xs">{agent.role}</td>
                        <td className="whitespace-nowrap text-xs"><AgentTypeLabel agent={agent} catalog={catalog} /></td>
                        <td><StatusLabel tone={status.tone} title={status.detail}>{status.label}</StatusLabel></td>
                        <td>
                          {!readOnly && (
                            <Inline gap="xs" nowrap className="justify-end">
                              {at && !agent.finished_at && agent.role !== 'planner' && (
                                <Button size="sm" onClick={() => setMessaging(agent)}>Message</Button>
                              )}
                              {!agent.hosted && <Button size="sm" onClick={() => showToken(agent)}>Show token</Button>}
                              {!started && <Button size="sm" variant="danger" onClick={() => setRemoving(agent)}>Remove</Button>}
                            </Inline>
                          )}
                        </td>
                      </tr>
                      {expanded && (
                        <tr>
                          <td colSpan={6} className="bg-surface-inset">
                            <AgentDetails agent={agent} messages={sent} catalog={catalog} />
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
        {messaging && (
          <MessageAgent pe={pe} agent={messaging} onSent={() => mutateMessages()} onClose={() => setMessaging(null)} />
        )}
        {removing && (
          <ConfirmDialog
            open
            title={`Remove ${removing.name} from this experiment?`}
            confirmLabel="Remove"
            onConfirm={() => remove(removing)}
            onClose={() => setRemoving(null)}
          >
            Its token stops working{removing.hosted ? ' and its machine is taken down' : ''}.
          </ConfirmDialog>
        )}
    </Section>
  )
}
