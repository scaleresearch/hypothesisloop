import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { fetchAgentTypes } from '@/lib/api/agent-types'
import { fetchPlatformExperimentAgents } from '@/lib/api/agents'
import { fetchModels } from '@/lib/api/models'
import { updatePlatformExperiment } from '@/lib/api/platform-experiments'
import { ApiError } from '@/lib/api/request'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import { Team } from '@/components/team'
import { CATALOG } from '@/test-agent-types'
import { MODELS, pickModel } from '@/test-models'
import { choose } from '@/test-select'
import type { JoinedAgent, Me, PlatformExperiment } from '@/types/api'

vi.mock('@/lib/api/agent-types', () => ({ fetchAgentTypes: vi.fn(), fetchAgentRoles: vi.fn(async () => (await import('@/test-agent-types')).ROLES) }))
vi.mock('@/lib/api/agents', () => ({ fetchPlatformExperimentAgents: vi.fn() }))
vi.mock('@/lib/api/models', () => ({ fetchModels: vi.fn() }))
vi.mock('@/lib/api/platform-experiments', () => ({ updatePlatformExperiment: vi.fn() }))

const at = '2026-01-01T00:00:00Z'
const me: Me = {
  kind: 'user', user_id: 'u1', name: 'Ada',
  orgs: [{ id: 'org_a', name: 'Lab', role: 'owner', created_at: at, actions: ['edit', 'create_api_key'] }],
  projects: [{ id: 'prj_1', org_id: 'org_a', name: 'default', created_at: at }],
}

function agent(id: string, hour: number, agentType = 'generalist', more: Partial<JoinedAgent> = {}): JoinedAgent {
  return {
    platform_experiment_id: 'pe-1', agent_id: id, name: id, role: 'executor', actor: { kind: 'user', id: 'u1', name: 'Ada' },
    in_agentsandbox: true, agent_type: agentType, model: 'anthropic/claude-sonnet-5', hyperparameters: null, at_work: true, ended: '', signed_up_at: `2026-01-01T0${hour}:00:00Z`, actions: [], ...more,
  }
}

const pe = {
  id: 'pe-1', project_id: 'prj_1', status: 'running', max_agents: 4, editable: ['name', 'description', 'duration_hours', 'agents'],
  agents: [{ agent_type: 'generalist', model: 'anthropic/claude-sonnet-5', count: 2 }, { agent_type: 'at-lr', model: 'anthropic/claude-sonnet-5', count: 1 }, { agent_type: 'researcher', count: 1 }],
  agents_at_work: { total: 3, pool: [{ agent_type: 'generalist', model: 'anthropic/claude-sonnet-5', at_work: 2 }, { agent_type: 'at-lr', model: 'anthropic/claude-sonnet-5', at_work: 1 }, { agent_type: 'researcher', at_work: 1 }], outside_team: 0 },
  coordinator: { model: 'anthropic/claude-opus-5-5' },
} as unknown as PlatformExperiment

function show(experiment = pe) {
  const resolved = resolveTenant(me, '')
  if (resolved.state !== 'selected') throw new Error('no tenant')
  return render(
    <SWRConfig value={{ provider: () => new Map() }}>
      <TenantContext.Provider value={resolved.tenant}>
        <Team pe={experiment} />
      </TenantContext.Provider>
    </SWRConfig>,
  )
}

beforeEach(() => {
  vi.mocked(fetchAgentTypes).mockResolvedValue(CATALOG)
  vi.mocked(fetchModels).mockResolvedValue(MODELS)
  vi.mocked(fetchPlatformExperimentAgents).mockResolvedValue([
    agent('ag-old', 1), agent('ag-mid', 2), agent('ag-lr', 3, 'at-lr'), agent('ag-r', 1, 'researcher', { role: 'researcher', model: '' }),
    agent('ag-cut', 0, 'generalist', { at_work: false, ended_reason: 'cut' }),
  ])
  vi.mocked(updatePlatformExperiment).mockReset().mockImplementation(async (_id, patch) => ({ ...pe, ...patch }) as PlatformExperiment)
})

describe('the team of an experiment', () => {
  it('shows each entry by type name and role with how many are at work, a stage cut not counted', async () => {
    show()
    expect(await screen.findByText('3 executors at work of 4 max')).toBeTruthy()
    await screen.findByText('LR sweeper')
    const rows = within(screen.getByRole('table', { name: 'Team' })).getAllByRole('row').slice(1).map(r => r.textContent)
    // A researcher's entry that names no model shows the one it runs on: the coordinator's.
    await waitFor(() => expect(within(screen.getByRole('table', { name: 'Team' })).getAllByRole('row').slice(1).map(r => r.textContent)).toEqual([
      'GeneralistExecutorclaude-sonnet-5Claude22', 'LR sweeperExecutorclaude-sonnet-5Claude11', "ResearcherResearcherclaude-opus-5-5Claude(coordinator's model)11",
    ]))
  })

  it("shows an entry on its type's model, and one on no model of its own on the platform's default", async () => {
    show({ ...pe, agents: [{ agent_type: 'at-lr', count: 1 }, { agent_type: 'generalist', count: 1 }], agents_at_work: { total: 0, pool: [], outside_team: 0 } })
    await waitFor(() => expect(within(screen.getByRole('table', { name: 'Team' })).getAllByRole('row').slice(1).map(r => r.textContent)).toEqual([
      "LR sweeperExecutormoonshotai/kimi-k2OpenRouter(type's model)10",
      'GeneralistExecutorclaude-sonnet-5Claude(platform default)10',
    ]))
  })

  it('counts what the coordinator added beyond the team on its own row', async () => {
    show({ ...pe, agents_at_work: { ...pe.agents_at_work!, total: 5, outside_team: 2 } })
    expect(await screen.findByText('5 executors at work of 4 max')).toBeTruthy()
    const rows = within(screen.getByRole('table', { name: 'Team' })).getAllByRole('row').slice(1).map(r => r.textContent)
    expect(rows.at(-1)).toBe('Added by the coordinator beyond the team2')
  })

  it('confirms a team of fewer executors than are at work, naming which, the newest, are cut, never a researcher', async () => {
    show()
    await userEvent.click(await screen.findByRole('button', { name: 'Edit' }))
    expect(screen.queryByRole('spinbutton', { name: 'Max executors' })).toBeNull()
    await userEvent.click(within(screen.getByRole('listitem', { name: 'Team entry 2' })).getByRole('button', { name: 'Remove team entry 2' }))
    const count = screen.getByRole('spinbutton', { name: 'Team entry 1 count' })
    await userEvent.clear(count)
    await userEvent.type(count, '1')
    expect(screen.getByText('Saving cuts the 2 newest executors at work.')).toBeTruthy()
    await userEvent.click(screen.getByRole('button', { name: 'Save' }))
    const dialog = within(screen.getByRole('dialog'))
    expect(dialog.getByText('Cut 2 executors at work?')).toBeTruthy()
    expect(dialog.getAllByRole('listitem').map(li => li.firstChild?.textContent)).toEqual(['ag-mid', 'ag-lr'])
    expect(updatePlatformExperiment).not.toHaveBeenCalled()
    await userEvent.click(dialog.getByRole('button', { name: 'Save and cut 2' }))
    expect(updatePlatformExperiment).toHaveBeenCalledExactlyOnceWith('pe-1', {
      agents: [{ agent_type: 'generalist', model: 'anthropic/claude-sonnet-5', count: 1 }, { agent_type: 'researcher', count: 1 }],
    })
  })

  it('sends a larger team, without asking, an entry keeping its hyperparameters, the coordinator shown first', async () => {
    show({ ...pe, agents: [{ ...pe.agents![0], hyperparameters: { direction: 'kernels' } }, ...pe.agents!.slice(1)] })
    await userEvent.click(await screen.findByRole('button', { name: 'Edit' }))
    const coordinator = screen.getByRole('listitem', { name: 'Coordinator' })
    expect(within(coordinator).getByLabelText('Coordinator model')).toHaveProperty('disabled', true)
    expect(screen.getByText('Starts from: kernels')).toBeTruthy()
    // A researcher's entry defaults to the coordinator's model when neither it nor its type names one.
    expect((screen.getByLabelText('Team entry 3 model') as HTMLInputElement).placeholder).toBe("Coordinator's model: anthropic/claude-opus-5-5")
    await userEvent.click(screen.getByRole('button', { name: '+ Add executor' }))
    await choose(screen.getByLabelText('Team entry 4 type'), 'Data search')
    const count = screen.getByRole('spinbutton', { name: 'Team entry 4 count' })
    await userEvent.clear(count)
    await userEvent.type(count, '3')
    await userEvent.click(screen.getByRole('button', { name: 'Save' }))
    await waitFor(() => expect(updatePlatformExperiment).toHaveBeenCalledExactlyOnceWith('pe-1', {
      agents: [
        { agent_type: 'generalist', model: 'anthropic/claude-sonnet-5', count: 2, hyperparameters: { direction: 'kernels' } },
        { agent_type: 'at-lr', model: 'anthropic/claude-sonnet-5', count: 1 },
        { agent_type: 'researcher', count: 1 },
        { agent_type: 'data-search', count: 3 },
      ],
    }))
    expect(screen.queryByRole('dialog')).toBeNull()
  })

  it('sends an entry below its agents at work and shows the API\'s refusal of it, staying open', async () => {
    const refusal = '2 Generalist executors on m are at work: keep at least 2.'
    vi.mocked(updatePlatformExperiment).mockRejectedValue(new ApiError(422, refusal, 'agents_below_at_work', {
      reason: 'agents_below_at_work', message: refusal, problems: [{ reason: 'agents_below_at_work', message: refusal, field: 'agents' }],
    }))
    show({ ...pe, status: 'open' })
    await userEvent.click(await screen.findByRole('button', { name: 'Edit' }))
    const count = screen.getByRole('spinbutton', { name: 'Team entry 1 count' })
    await userEvent.clear(count)
    await userEvent.type(count, '1')
    await userEvent.click(screen.getByRole('button', { name: 'Save' }))
    expect(await screen.findByText(refusal)).toBeTruthy()
    expect(updatePlatformExperiment).toHaveBeenCalledExactlyOnceWith('pe-1', {
      agents: [{ agent_type: 'generalist', model: 'anthropic/claude-sonnet-5', count: 1 }, { agent_type: 'at-lr', model: 'anthropic/claude-sonnet-5', count: 1 }, { agent_type: 'researcher', count: 1 }],
    })
    expect(screen.getByRole('spinbutton', { name: 'Team entry 1 count' })).toBeTruthy()
  })

  it('names the model sent on the entry a refusal names, which the API does not repeat', async () => {
    const refusal = 'agents[3].model is not served: the model is not one GET /models lists: openrouter serves no model of that id that this platform runs an agent on'
    vi.mocked(updatePlatformExperiment).mockRejectedValue(new ApiError(422, refusal, 'model_not_served', {
      reason: 'model_not_served', message: refusal, problems: [{ reason: 'model_not_served', message: refusal, field: 'agents[3].model' }],
    }))
    show()
    await userEvent.click(await screen.findByRole('button', { name: 'Edit' }))
    await userEvent.click(screen.getByRole('button', { name: '+ Add executor' }))
    await pickModel(screen.getByLabelText('Team entry 4 model'), 'kimi', 'openrouter/moonshotai/kimi-k2')
    await userEvent.click(screen.getByRole('button', { name: 'Save' }))
    expect(await screen.findByText(/^agents\[3\]\.model openrouter\/moonshotai\/kimi-k2 is not served: /)).toBeTruthy()
  })

  it('offers no edit where the API lists no agents in editable: to a reader, or on a closed one', async () => {
    const { unmount } = show({ ...pe, editable: [] })
    await screen.findByText('3 executors at work of 4 max')
    expect(screen.queryByRole('button', { name: 'Edit' })).toBeNull()
    unmount()
    show({ ...pe, status: 'closed', editable: [] })
    await screen.findByText('3 entries')
    expect(screen.queryByRole('button', { name: 'Edit' })).toBeNull()
  })
})
