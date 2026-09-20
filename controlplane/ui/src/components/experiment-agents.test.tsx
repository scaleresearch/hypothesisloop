import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import {
  addPlatformExperimentAgents, fetchPlatformExperimentAgents, fetchPlatformExperimentAgentToken, removePlatformExperimentAgent,
} from '@/lib/api'
import { ExperimentAgents } from '@/components/experiment-agents'
import { PlatformExperimentStatus } from '@/types'
import type { AgentSignup, PlatformExperiment } from '@/types'

vi.mock('@/lib/api', () => ({
  addPlatformExperimentAgents: vi.fn(), fetchPlatformExperimentAgents: vi.fn(),
  fetchPlatformExperimentAgentToken: vi.fn(), removePlatformExperimentAgent: vi.fn(),
}))

const at = '2026-01-01T00:00:00Z'
let roster: AgentSignup[]

function signup(id: string, hosted: boolean, flavor?: string): AgentSignup {
  return { platform_experiment_id: 'pe-1', agent_id: id, name: id, role: 'competitor', hosted, flavor, signed_up_at: at }
}

function show(status = PlatformExperimentStatus.OPEN) {
  const pe = { id: 'pe-1', project_id: 'prj_1', status } as PlatformExperiment
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
  vi.stubGlobal('confirm', vi.fn(() => true))
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
    await userEvent.type(screen.getByLabelText('Flavor'), 'data-search')
    await userEvent.type(screen.getByLabelText('Model'), 'claude-opus-4-5')
    await userEvent.click(screen.getByRole('button', { name: 'Me' }))
    await userEvent.click(screen.getByRole('button', { name: 'Add agent' }))
    expect(addPlatformExperimentAgents).toHaveBeenCalledExactlyOnceWith('pe-1', {
      hosted: false, flavor: 'data-search', model: 'claude-opus-4-5',
    })
    const secrets = await screen.findAllByTestId('secret')
    expect(secrets.map(s => s.textContent)).toEqual([
      'AGENT_ID=ag-new1\nHL_TOKEN=hls_token-of-ag-new1\nPLATFORM_EXPERIMENT_ID=pe-1\nAGENT_FLAVOR=data-search',
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
    expect(screen.getByText(/brings the same one back/)).toBeTruthy()
    expect(screen.getByText('hl platform-experiments agents add pe-1 --agent ag-new1')).toBeTruthy()
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
    expect(confirm).toHaveBeenCalledExactlyOnceWith(expect.stringContaining('Remove ag-old'))
    expect(removePlatformExperimentAgent).toHaveBeenCalledExactlyOnceWith('pe-1', 'ag-old')
    await waitFor(() => expect(screen.queryByRole('cell', { name: 'ag-old' })).toBeNull())
  })

  it('does nothing when the answer is no', async () => {
    vi.mocked(confirm).mockReturnValue(false)
    show()
    await userEvent.click(await screen.findByRole('button', { name: 'Remove' }))
    expect(removePlatformExperimentAgent).not.toHaveBeenCalled()
  })

  it('shows why the API refused, as when the experiment started in the meantime', async () => {
    vi.mocked(removePlatformExperimentAgent).mockRejectedValue(new Error('experiment_started: experiment is running'))
    show()
    await userEvent.click(await screen.findByRole('button', { name: 'Remove' }))
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
