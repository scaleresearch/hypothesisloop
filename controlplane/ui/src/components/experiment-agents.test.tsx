import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { fetchAgentTypes } from '@/lib/api/agent-types'
import { fetchModels } from '@/lib/api/models'
import { addPlatformExperimentAgents, fetchPlatformExperimentAgents, fetchPlatformExperimentAgentToken, fetchPlatformExperimentMessages, removePlatformExperimentAgent, sendPlatformExperimentMessage } from '@/lib/api/agents'
import { createPlatformExperiment } from '@/lib/api/platform-experiments'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import { choose } from '@/test-select'
import { CATALOG } from '@/test-agent-types'
import { MODELS, pickModel } from '@/test-models'
import { ExperimentAgents } from '@/components/experiment-agents'
import type { AgentMessage, JoinedAgent, Me, PlatformExperiment, PlatformExperimentAction, SignupAction } from '@/types/api'

class ApiError extends Error {
  constructor(readonly status: number, message: string, readonly reason?: string) { super(message) }
}

vi.mock('@/lib/api/agent-types', () => ({ fetchAgentTypes: vi.fn(), fetchAgentRoles: vi.fn(async () => (await import('@/test-agent-types')).ROLES) }))
vi.mock('@/lib/api/models', () => ({ fetchModels: vi.fn() }))
vi.mock('@/lib/api/agents', () => ({
  addPlatformExperimentAgents: vi.fn(), fetchPlatformExperimentAgents: vi.fn(), fetchPlatformExperimentAgentToken: vi.fn(), removePlatformExperimentAgent: vi.fn(), fetchPlatformExperimentMessages: vi.fn(), sendPlatformExperimentMessage: vi.fn(),
}))
vi.mock('@/lib/api/platform-experiments', () => ({ createPlatformExperiment: vi.fn() }))

const at = '2026-01-01T00:00:00Z'
let roster: JoinedAgent[]
let shownStatus = 'open'

const me: Me = {
  kind: 'user', user_id: 'u1', name: 'Ada',
  orgs: [{ id: 'org_a', name: 'Lab', role: 'owner', created_at: at, actions: ['edit', 'create_api_key'] }],
  projects: [{ id: 'prj_1', org_id: 'org_a', name: 'default', created_at: at }],
}

function signup(id: string, sandboxed: boolean, agentType = ''): JoinedAgent {
  return { platform_experiment_id: 'pe-1', agent_id: id, name: id, role: 'competitor', actor: { kind: 'user', id: 'u1', name: 'Ada' }, in_agentsandbox: sandboxed, agent_type: agentType, model: '', hyperparameters: null, at_work: true, ended: '', signed_up_at: at, actions: [] }
}

// As the API answers an agent to the experiment's owner.
function asListed(agent: JoinedAgent): JoinedAgent {
  const actions: SignupAction[] = []
  if (agent.role !== 'planner' && !agent.finished_at && shownStatus !== 'closed') actions.push('message')
  if (shownStatus === 'open') actions.push('remove')
  if (!agent.in_agentsandbox) actions.push('show_token')
  return { ...agent, actions }
}

// As the API lists them to the experiment's owner: a researcher joins it while it is open or runs.
function ownerActions(status: string): PlatformExperimentAction[] {
  if (status === 'closed') return ['remove']
  return ['remove', 'close', 'add_idea', 'add_agent']
}

function show(status = 'open', more: Partial<PlatformExperiment> = {}) {
  shownStatus = status
  const pe = { id: 'pe-1', project_id: 'prj_1', status, editable: [], actions: ownerActions(status), ...more } as PlatformExperiment
  const resolved = resolveTenant(me, '')
  if (resolved.state !== 'selected') throw new Error('no tenant')
  return render(
    <SWRConfig value={{ provider: () => new Map() }}>
      <TenantContext.Provider value={resolved.tenant}><ExperimentAgents pe={pe} /></TenantContext.Provider>
    </SWRConfig>,
  )
}

beforeEach(() => {
  vi.mocked(fetchModels).mockResolvedValue(MODELS)
  roster = [signup('ag-old', true)]
  vi.mocked(fetchPlatformExperimentAgents).mockImplementation(async () => roster.map(asListed))
  vi.mocked(addPlatformExperimentAgents).mockImplementation(async (_pe, req) => {
    const added = signup(`ag-new${roster.length}`, !!req.in_agentsandbox, req.agent_type)
    roster = [...roster, added]
    return [asListed(req.in_agentsandbox ? added : { ...added, token: `hls_token-of-${added.agent_id}` })]
  })
  vi.mocked(fetchPlatformExperimentAgentToken).mockImplementation(async (_pe, id) => (
    asListed({ ...roster.find(a => a.agent_id === id)!, token: `hls_token-of-${id}` })
  ))
  vi.mocked(removePlatformExperimentAgent).mockImplementation(async (_pe, id) => {
    roster = roster.filter(a => a.agent_id !== id)
  })
  vi.mocked(fetchPlatformExperimentMessages).mockResolvedValue([])
  vi.mocked(fetchAgentTypes).mockResolvedValue(CATALOG)
})

describe('messaging an agent', () => {
  it('sends the owner\'s message and shows the messages the agent was sent', async () => {
    const sent: AgentMessage[] = [{ agent_id: 'ag-old', from: 'coordinator', text: 'leave the lr line', actor: { kind: 'coordinator', id: 'pe-1', name: 'pe-1' }, at }]
    vi.mocked(fetchPlatformExperimentMessages).mockResolvedValue(sent)
    vi.mocked(sendPlatformExperimentMessage).mockImplementation(async (_pe, agent, text) => {
      const one: AgentMessage = { ...sent[0], agent_id: agent, from: 'owner', text }
      sent.push(one)
      return [one]
    })
    show('running')
    await userEvent.click(await screen.findByRole('button', { name: 'Message' }))
    await userEvent.type(screen.getByRole('textbox', { name: 'Message' }), 'try warmup')
    await userEvent.click(screen.getByRole('button', { name: 'Send' }))
    expect(sendPlatformExperimentMessage).toHaveBeenCalledExactlyOnceWith('pe-1', 'ag-old', 'try warmup')
    await userEvent.click(screen.getByRole('button', { name: 'Details of ag-old' }))
    expect(await screen.findByText(/From the owner, .*: try warmup/)).toBeTruthy()
    expect(screen.getByText(/From the coordinator, .*: leave the lr line/)).toBeTruthy()
  })

  it('offers no message to an agent that finished or in a closed experiment', async () => {
    roster = [{ ...signup('ag-old', true), ended: 'finished', finished_at: at, finished_reason: 'quota spent' }, signup('ag-2', true)]
    const { unmount } = show('running')
    expect(await screen.findAllByRole('button', { name: 'Message' })).toHaveLength(1)
    unmount()
    show('closed')
    await screen.findByRole('cell', { name: 'ag-2' })
    expect(screen.queryByRole('button', { name: 'Message' })).toBeNull()
  })
})

describe('adding agents', () => {
  it('asks the platform to run them by default and shows no token', async () => {
    show()
    await screen.findByRole('button', { name: 'Executor' })
    await userEvent.click(screen.getByRole('button', { name: 'Add agent' }))
    expect(addPlatformExperimentAgents).toHaveBeenCalledExactlyOnceWith('pe-1', { in_agentsandbox: true, agent_type: 'generalist', role: 'competitor', model: undefined })
    expect(await screen.findByRole('cell', { name: 'ag-new1' })).toBeTruthy()
    expect(screen.queryByTestId('secret')).toBeNull()
  })

  it('adds one agent a click, with agent type and model, and shows the self-run agent\'s token', async () => {
    show()
    await choose(await screen.findByLabelText('Type'), 'Data search')
    await pickModel(screen.getByLabelText('Model'), 'kimi', 'openrouter/moonshotai/kimi-k2')
    await userEvent.click(screen.getByRole('button', { name: 'Me' }))
    await userEvent.click(screen.getByRole('button', { name: 'Add agent' }))
    expect(addPlatformExperimentAgents).toHaveBeenCalledExactlyOnceWith('pe-1', {
      in_agentsandbox: false, agent_type: 'data-search', role: 'competitor', model: 'openrouter/moonshotai/kimi-k2',
    })
    const secrets = await screen.findAllByTestId('secret')
    expect(secrets.map(s => s.textContent)).toEqual([
      'API_URL=http://api.test\nHL_TOKEN=hls_token-of-ag-new1',
    ])
  })

  it('adds a researcher as its role, which runs the researcher type on the coordinator\'s model unless one is picked', async () => {
    show()
    await userEvent.click(await screen.findByRole('button', { name: 'Researcher' }))
    expect(screen.queryByLabelText('Type')).toBeNull()
    expect(screen.getByText(/Keeps the hypothesis graph correct/)).toBeTruthy()
    expect((screen.getByLabelText('Model') as HTMLInputElement).placeholder).toBe("Coordinator's model: anthropic/claude-opus-5-5")
    await userEvent.click(screen.getByRole('button', { name: 'Add agent' }))
    expect(addPlatformExperimentAgents).toHaveBeenCalledExactlyOnceWith('pe-1', { in_agentsandbox: true, agent_type: 'researcher', role: 'researcher', model: undefined })
  })

  it('hides a token on request, lists none in the roster, and shows the same one again when asked', async () => {
    show()
    await userEvent.click(screen.getByRole('button', { name: 'Me' }))
    await userEvent.click(screen.getByRole('button', { name: 'Add agent' }))
    const first = (await screen.findByTestId('secret')).textContent
    await userEvent.click(screen.getByRole('button', { name: 'Hide' }))
    expect(document.body.textContent).not.toContain('hls_')

    const row = screen.getByRole('cell', { name: 'ag-new1' }).closest('tr')!
    await userEvent.click(within(row).getByRole('button', { name: 'Show token' }))
    expect(fetchPlatformExperimentAgentToken).toHaveBeenCalledExactlyOnceWith('pe-1', 'ag-new1')
    expect((await screen.findByTestId('secret')).textContent).toBe(first)
    expect(screen.getAllByTestId('secret')).toHaveLength(1)
  })

  it('offers no token for an agent the platform runs', async () => {
    show()
    const row = (await screen.findByRole('cell', { name: 'ag-old' })).closest('tr')!
    expect(within(row).queryByRole('button', { name: 'Show token' })).toBeNull()
  })

  it('shows why a token could not be fetched', async () => {
    roster = [signup('ag-self', false)]
    vi.mocked(fetchPlatformExperimentAgentToken).mockRejectedValue(new Error('internal error'))
    show()
    await userEvent.click(await screen.findByRole('button', { name: 'Show token' }))
    expect(await screen.findByText('internal error')).toBeTruthy()
    expect(screen.queryByTestId('secret')).toBeNull()
  })

  it('shows the API\'s refusal', async () => {
    vi.mocked(addPlatformExperimentAgents).mockRejectedValue(new Error('max_agents_reached: limit is 1'))
    show()
    await userEvent.click(screen.getByRole('button', { name: 'Add agent' }))
    expect(await screen.findByText('max_agents_reached: limit is 1')).toBeTruthy()
  })
})

describe('removing an agent', () => {
  it('asks first, then removes it from the roster', async () => {
    show()
    await userEvent.click(await screen.findByRole('button', { name: 'Remove' }))
    const question = within(screen.getByRole('dialog'))
    expect(question.getByText('Remove ag-old from this experiment?')).toBeTruthy()
    await userEvent.click(question.getByRole('button', { name: 'Remove' }))
    expect(removePlatformExperimentAgent).toHaveBeenCalledExactlyOnceWith('pe-1', 'ag-old')
    await waitFor(() => expect(screen.queryByRole('cell', { name: 'ag-old' })).toBeNull())
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
  })

  it('does nothing when the answer is no', async () => {
    show()
    await userEvent.click(await screen.findByRole('button', { name: 'Remove' }))
    await userEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Cancel' }))
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(removePlatformExperimentAgent).not.toHaveBeenCalled()
  })

  it('says in the API\'s words that an agent with a job queued or running stays', async () => {
    const unfinished = 'agent ag-old has a job queued or running, and its quota, which that job is charged to, goes with the agent: cancel the job (`hl job cancel`) or wait for it to end, then remove the agent'
    vi.mocked(removePlatformExperimentAgent).mockRejectedValue(new ApiError(409, unfinished, 'work_unfinished'))
    show()
    await userEvent.click(await screen.findByRole('button', { name: 'Remove' }))
    await userEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Remove' }))
    const alert = await screen.findByRole('alert')
    expect(alert.textContent).toBe(unfinished)
    expect(screen.getByRole('dialog').contains(alert)).toBe(true)
    expect(screen.getByRole('cell', { name: 'ag-old' })).toBeTruthy()
  })

  it('shows why the API refused, as when the experiment started in the meantime', async () => {
    vi.mocked(removePlatformExperimentAgent).mockRejectedValue(new Error('experiment_started: experiment is running'))
    show()
    await userEvent.click(await screen.findByRole('button', { name: 'Remove' }))
    await userEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Remove' }))
    expect(await screen.findByText('experiment_started: experiment is running')).toBeTruthy()
  })
})

describe('once the experiment has started', () => {
  it('takes a researcher in the place of one while it runs, and removes nobody', async () => {
    show('running')
    expect(await screen.findByRole('cell', { name: 'ag-old' })).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Add agent' })).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Remove' })).toBeNull()
  })

  it('shows the roster of a closed one and nothing to change it with', async () => {
    show('closed')
    expect(await screen.findByRole('cell', { name: 'ag-old' })).toBeTruthy()
    expect(screen.queryByRole('button', { name: /^Add/ })).toBeNull()
    expect(screen.queryByRole('button', { name: 'Remove' })).toBeNull()
  })

  it('still shows a self-run agent\'s token, which its operator may need to restart it', async () => {
    roster = [signup('ag-self', false)]
    show('running')
    await userEvent.click(await screen.findByRole('button', { name: 'Show token' }))
    expect((await screen.findByTestId('secret')).textContent).toContain('HL_TOKEN=hls_token-of-ag-self')
  })
})

describe('an experiment waiting to start', () => {
  const waits = { reason: 'baseline_required', waits_for: 'coordinator' as const, summary: 'No usable baseline is measured yet.', message: 'Add a baseline agent with `hl platform-experiments agents add --in-agentsandbox --role baseline --platform-experiment pe-1`: none has joined. platform experiment pe-1 has no baseline declared' }

  it('offers nothing to start it with', async () => {
    show('open', { status_reason: waits })
    await screen.findByRole('cell', { name: 'ag-old' })
    expect(screen.queryByRole('button', { name: /^Start/ })).toBeNull()
  })
})

describe('agent types in the roster', () => {
  async function row(id: string) {
    return within((await screen.findByRole('cell', { name: id })).closest('tr')!)
  }

  it('names each agent\'s role as the API names roles', async () => {
    roster = [signup('ag-1', true, 'generalist'), { ...signup('ag-2', true, 'researcher'), role: 'researcher' }]
    show('running')
    expect((await row('ag-1')).getAllByRole('cell')[2].textContent).toBe('Executor')
    expect((await row('ag-2')).getAllByRole('cell')[2].textContent).toBe('Researcher')
  })

  it('names each agent\'s type, an unknown type by its id', async () => {
    roster = [signup('ag-1', true, 'at-lr'), signup('ag-2', true, 'data-search'), signup('ag-3', true, 'at-gone')]
    show('running')
    await screen.findByText('LR sweeper')
    expect((await row('ag-1')).getByText('LR sweeper')).toBeTruthy()
    expect((await row('ag-2')).getByText('Data search')).toBeTruthy()
    expect((await row('ag-3')).getByText('at-gone')).toBeTruthy()
  })

  // The row read where an agent stands from the control plane's rules copied into the UI; the API
  // now says it in ended, and its machine's state only matters while ended is empty.
  it('labels each agent by the ended the API lists, over the state of its machine', async () => {
    const machine = { state: 'running' as const, restarts: 0, stopped: false }
    roster = [
      { ...signup('ag-finished', true, 'generalist'), ended: 'finished', finished_at: at, instance: machine },
      { ...signup('ag-cut', true, 'generalist'), ended: 'cut', instance: machine },
      { ...signup('ag-closed', true, 'generalist'), ended: 'closed', instance: machine },
      { ...signup('ag-planned', true, 'planner'), role: 'planner', ended: 'planned', instance: machine },
      { ...signup('ag-working', true, 'generalist'), instance: machine },
    ]
    show('running')
    expect((await row('ag-finished')).getByText(/^Finished /)).toBeTruthy()
    expect((await row('ag-cut')).getByText('Cut at a stage boundary')).toBeTruthy()
    expect((await row('ag-closed')).getByText('Stopped, experiment closed')).toBeTruthy()
    expect((await row('ag-planned')).getByText('Answered the draft')).toBeTruthy()
    expect((await row('ag-working')).getByText('Running')).toBeTruthy()
  })

  it('says an executor was cut when max executors was lowered', async () => {
    roster = [{ ...signup('ag-1', true, 'generalist'), ended: 'cut', cut_reason: 'max_agents_lowered' }]
    show('running')
    expect(await screen.findByText('Cut, max executors lowered')).toBeTruthy()
  })
})
