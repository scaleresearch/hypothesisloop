import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import {
  addPlatformExperimentAgents, fetchPlatformExperimentAgents, fetchPlatformExperimentAgentToken, removePlatformExperimentAgent,
  createPlatformExperiment,
} from '@/lib/api'
import { ExperimentAgents } from '@/components/experiment-agents'
import { clearRunFailure, createAndRun, runFailure } from '@/lib/run-experiment'
import { PlatformExperimentStatus } from '@/types'
import type { AgentSignup, JoinedAgent, PlatformExperiment } from '@/types'

vi.mock('@/lib/api', () => ({
  apiURL: () => 'http://api.test',
  createPlatformExperiment: vi.fn(),
  addPlatformExperimentAgents: vi.fn(), fetchPlatformExperimentAgents: vi.fn(),
  fetchPlatformExperimentAgentToken: vi.fn(), removePlatformExperimentAgent: vi.fn(),
}))

const at = '2026-01-01T00:00:00Z'
let roster: JoinedAgent[]

function signup(id: string, hosted: boolean, flavor?: string): AgentSignup {
  return { platform_experiment_id: 'pe-1', agent_id: id, name: id, role: 'competitor', actor: { kind: 'user', id: 'u1', name: 'Ada' }, hosted, flavor, signed_up_at: at }
}

function show(status = PlatformExperimentStatus.OPEN, more: Partial<PlatformExperiment> = {}) {
  const pe = { id: 'pe-1', project_id: 'prj_1', status, ...more } as PlatformExperiment
  return render(
    <SWRConfig value={{ provider: () => new Map() }}><ExperimentAgents pe={pe} /></SWRConfig>,
  )
}

beforeEach(() => {
  roster = [signup('ag-old', true)]
  vi.mocked(fetchPlatformExperimentAgents).mockImplementation(async () => [...roster])
  vi.mocked(addPlatformExperimentAgents).mockImplementation(async (_pe, req) => {
    const added = signup(`ag-new${roster.length}`, req.hosted, req.flavor)
    roster = [...roster, added]
    return [req.hosted ? added : { ...added, token: `hls_token-of-${added.agent_id}` }]
  })
  vi.mocked(fetchPlatformExperimentAgentToken).mockImplementation(async (_pe, id) => (
    { ...roster.find(a => a.agent_id === id)!, token: `hls_token-of-${id}` }
  ))
  vi.mocked(removePlatformExperimentAgent).mockImplementation(async (_pe, id) => {
    roster = roster.filter(a => a.agent_id !== id)
  })
})

describe('adding agents', () => {
  it('asks the platform to run them by default and shows no token', async () => {
    show()
    await userEvent.click(screen.getByRole('button', { name: 'Add agent' }))
    expect(addPlatformExperimentAgents).toHaveBeenCalledExactlyOnceWith('pe-1', { hosted: true, flavor: undefined, model: undefined })
    expect(await screen.findByRole('cell', { name: 'ag-new1' })).toBeTruthy()
    expect(screen.queryByTestId('secret')).toBeNull()
  })

  it('adds one agent a click, with flavor and model, and shows the self-run agent\'s token', async () => {
    show()
    await userEvent.type(screen.getByLabelText('Specialization'), 'data-search')
    await userEvent.type(screen.getByLabelText('Model'), 'claude-opus-4-5')
    await userEvent.click(screen.getByRole('button', { name: 'Me' }))
    await userEvent.click(screen.getByRole('button', { name: 'Add agent' }))
    expect(addPlatformExperimentAgents).toHaveBeenCalledExactlyOnceWith('pe-1', {
      hosted: false, flavor: 'data-search', model: 'claude-opus-4-5',
    })
    const secrets = await screen.findAllByTestId('secret')
    expect(secrets.map(s => s.textContent)).toEqual([
      'API_URL=http://api.test\nHL_TOKEN=hls_token-of-ag-new1',
    ])
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

  it('tells the truth about the token: it can be had again', async () => {
    show()
    await userEvent.click(screen.getByRole('button', { name: 'Me' }))
    await userEvent.click(screen.getByRole('button', { name: 'Add agent' }))
    await screen.findByTestId('secret')
    expect(screen.getByText(/shows the same one again/)).toBeTruthy()
    expect(screen.getByText('hl platform-experiments agents add --platform-experiment pe-1 --agent ag-new1')).toBeTruthy()
    expect(document.body.textContent).not.toMatch(/never again|only time|shown once/)
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
  })

  it('does nothing when the answer is no', async () => {
    show()
    await userEvent.click(await screen.findByRole('button', { name: 'Remove' }))
    await userEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Cancel' }))
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(removePlatformExperimentAgent).not.toHaveBeenCalled()
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
  it.each([PlatformExperimentStatus.RUNNING, PlatformExperimentStatus.CLOSED])('shows the roster and nothing to change it with: %s', async status => {
    show(status)
    expect(await screen.findByRole('cell', { name: 'ag-old' })).toBeTruthy()
    expect(screen.queryByRole('button', { name: /^Add/ })).toBeNull()
    expect(screen.queryByRole('button', { name: 'Remove' })).toBeNull()
  })

  it('still shows a self-run agent\'s token, which its operator may need to restart it', async () => {
    roster = [signup('ag-self', false)]
    show(PlatformExperimentStatus.RUNNING)
    await userEvent.click(await screen.findByRole('button', { name: 'Show token' }))
    expect((await screen.findByTestId('secret')).textContent).toContain('HL_TOKEN=hls_token-of-ag-self')
  })
})

describe('the machines the platform runs', () => {
  it('shows each hosted agent\'s machine in its row, and says a self-run agent has none of the platform\'s', async () => {
    roster = [
      { ...signup('ag-hosted', true), instance: { state: 'running', restarts: 1, stopped: false, progress_at: new Date().toISOString(), end_reason: 'gone', exited_at: new Date().toISOString(), exit_log: 'lost' } },
      signup('ag-new', true),
      signup('ag-own', false),
    ]
    show(PlatformExperimentStatus.RUNNING)
    const hosted = within((await screen.findByRole('cell', { name: 'ag-hosted' })).closest('tr')!)
    expect(hosted.getByText('running')).toBeTruthy()
    expect(hosted.getByText('restarted once')).toBeTruthy()
    expect(hosted.getByText(/Replaced: the machine before this one ended .*, its provider no longer lists it\./)).toBeTruthy()
    expect(hosted.getByRole('button', { name: 'Exit log' })).toBeTruthy()
    expect(within(screen.getByRole('cell', { name: 'ag-new' }).closest('tr')!).getByText('not reported yet')).toBeTruthy()
    expect(within(screen.getByRole('cell', { name: 'ag-own' }).closest('tr')!).getByText('not the platform\'s')).toBeTruthy()
    expect(screen.queryByText('finished')).toBeNull()
  })

  it('says which agent finished and why it gave', async () => {
    roster = [{ ...signup('ag-done', true), finished_at: new Date().toISOString(), finished_reason: 'quota spent' }, signup('ag-busy', true)]
    show(PlatformExperimentStatus.RUNNING)
    const done = within((await screen.findByRole('cell', { name: 'ag-done' })).closest('tr')!)
    expect(done.getByText('finished')).toBeTruthy()
    expect(done.getByText(/: quota spent$/)).toBeTruthy()
    expect(within(screen.getByRole('cell', { name: 'ag-busy' }).closest('tr')!).queryByText('finished')).toBeNull()
  })

  it('shows the coordinator the platform runs, with its model and its machine', async () => {
    show(PlatformExperimentStatus.RUNNING, {
      coordinator: { launcher: 'vast', model: 'claude-opus-4-5', instance: { state: 'starting', restarts: 0, stopped: false } },
    })
    const coordinator = within(await screen.findByRole('group', { name: 'Coordinator' }))
    expect(coordinator.getByText('claude-opus-4-5')).toBeTruthy()
    expect(coordinator.getByText('starting')).toBeTruthy()
    expect(coordinator.getByText('no progress seen yet')).toBeTruthy()
    expect(coordinator.queryByText(/restarted/)).toBeNull()
  })

  it('shows no coordinator for an experiment the platform runs none for', async () => {
    show()
    await screen.findByRole('cell', { name: 'ag-old' })
    expect(screen.queryByRole('group', { name: 'Coordinator' })).toBeNull()
  })
})

describe('a machine on its way', () => {
  it('says beside the agent that its start is in progress, in the API\'s words', async () => {
    const starting = { reason: 'instance_starting', message: 'its machine was asked for 40s ago and is starting: a start in progress and no failure' }
    roster = [{ ...signup('ag-starting', true), instance: { state: 'starting', restarts: 0, stopped: false, asked_at: new Date().toISOString() }, status_reason: starting }]
    show(PlatformExperimentStatus.RUNNING)
    expect((await screen.findByText(starting.message)).closest('tr')).toBe(screen.getByRole('cell', { name: 'ag-starting' }).closest('tr'))
  })
})

describe('an experiment waiting to start', () => {
  const waits = { reason: 'baseline_required', actor: 'coordinator' as const, message: 'Add a baseline agent with `hl platform-experiments agents add --hosted --role baseline`: none has joined. platform experiment pe-1 has no baseline declared' }
  const agentWaits = { reason: 'baseline_required', message: 'its machine is asked for when the platform starts the run, and none is before: platform experiment pe-1 has no baseline declared' }
  const requested = { state: 'requested', restarts: 0, stopped: false } as const

  it('says the reader\'s next step in the API\'s words alone, and offers nothing to start it with', async () => {
    show(PlatformExperimentStatus.OPEN, { status_reason: waits })
    await screen.findByRole('cell', { name: 'ag-old' })
    expect(screen.getByRole('status').textContent).toBe(waits.message)
    expect(screen.queryByRole('button', { name: /^Start/ })).toBeNull()
  })

  it('says beside an agent whose machine is not asked for yet why it waits', async () => {
    roster = [
      { ...signup('ag-waits', true), instance: { ...requested, state: 'waiting' }, status_reason: agentWaits },
      { ...signup('ag-baseline', true), role: 'baseline', instance: requested },
    ]
    show(PlatformExperimentStatus.OPEN, { status_reason: waits })
    const row = (id: string) => within(screen.getByRole('cell', { name: id }).closest('tr')!)
    expect((await screen.findByText(agentWaits.message)).closest('tr')).toBe(screen.getByRole('cell', { name: 'ag-waits' }).closest('tr'))
    expect(row('ag-waits').getByText('waiting')).toBeTruthy()
    expect(row('ag-baseline').getByText('requested')).toBeTruthy()
    expect(row('ag-baseline').queryByText(/none is before/)).toBeNull()
  })

  it('says nothing of the kind once it runs', async () => {
    show(PlatformExperimentStatus.RUNNING)
    await screen.findByRole('cell', { name: 'ag-old' })
    expect(screen.queryByRole('status')).toBeNull()
  })
})

describe('an experiment the create form went on to run, and the API stopped part way', () => {
  beforeEach(async () => {
    clearRunFailure('pe-1')
    vi.mocked(createPlatformExperiment).mockResolvedValue({ id: 'pe-1' } as PlatformExperiment)
    vi.mocked(addPlatformExperimentAgents).mockRejectedValueOnce(new Error('the platform experiment names no repository'))
    await createAndRun('prj_1', {} as never, 2)
  })

  it('says which step failed in the API\'s words, and that nothing was undone', async () => {
    show()
    const said = await screen.findByText(/it is not running as asked: adding hosted agent 1 of 2 failed\. the platform experiment names no repository/)
    expect(said.textContent).toContain('Nothing was undone. Finish with the controls below.')
    expect(screen.getByRole('button', { name: 'Add agent' })).toBeTruthy()
  })

  it('stops saying so once dismissed', async () => {
    show()
    await userEvent.click(await screen.findByRole('button', { name: 'Dismiss' }))
    expect(screen.queryByText(/is not running as asked/)).toBeNull()
    expect(runFailure('pe-1')).toBeUndefined()
  })
})
