import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { fetchAgentTypes } from '@/lib/api/agent-types'
import { fetchPlatformExperimentAgents } from '@/lib/api/agents'
import { updatePlatformExperiment } from '@/lib/api/platform-experiments'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import { ResearcherPool } from '@/components/researcher-pool'
import { CATALOG } from '@/test-agent-types'
import { choose } from '@/test-select'
import type { JoinedAgent, Me, PlatformExperiment } from '@/types/api'

vi.mock('@/lib/api/agent-types', () => ({ fetchAgentTypes: vi.fn() }))
vi.mock('@/lib/api/agents', () => ({ fetchPlatformExperimentAgents: vi.fn() }))
vi.mock('@/lib/api/platform-experiments', () => ({ updatePlatformExperiment: vi.fn() }))

const at = '2026-01-01T00:00:00Z'
const me: Me = {
  kind: 'user', user_id: 'u1', name: 'Ada',
  orgs: [{ id: 'org_a', name: 'Lab', role: 'owner', created_at: at, actions: ['edit', 'create_api_key'] }],
  projects: [{ id: 'prj_1', org_id: 'org_a', name: 'default', created_at: at }],
}

function researcher(id: string, hour: number, agentType = 'generalist'): JoinedAgent {
  return {
    platform_experiment_id: 'pe-1', agent_id: id, name: id, role: 'competitor', actor: { kind: 'user', id: 'u1', name: 'Ada' },
    in_agentsandbox: true, agent_type: agentType, model: 'm', hyperparameters: null, at_work: true, signed_up_at: `2026-01-01T0${hour}:00:00Z`, actions: [],
  }
}

const pe = {
  id: 'pe-1', project_id: 'prj_1', status: 'running', max_agents: 4, editable: ['name', 'description', 'duration_hours', 'max_agents', 'researchers'],
  researchers: [{ agent_type: 'generalist', model: 'm', count: 2 }, { agent_type: 'at-lr', model: 'm', count: 1 }],
  researchers_at_work: { total: 3, pool: [{ agent_type: 'generalist', model: 'm', at_work: 2 }, { agent_type: 'at-lr', model: 'm', at_work: 1 }], outside_pool: 0 },
} as PlatformExperiment

function show(experiment = pe) {
  const resolved = resolveTenant(me, '')
  if (resolved.state !== 'selected') throw new Error('no tenant')
  return render(
    <SWRConfig value={{ provider: () => new Map() }}>
      <TenantContext.Provider value={resolved.tenant}>
        <ResearcherPool pe={experiment} />
      </TenantContext.Provider>
    </SWRConfig>,
  )
}

beforeEach(() => {
  vi.mocked(fetchAgentTypes).mockResolvedValue(CATALOG)
  vi.mocked(fetchPlatformExperimentAgents).mockResolvedValue([
    researcher('ag-old', 1), researcher('ag-mid', 2), researcher('ag-lr', 3, 'at-lr'), { ...researcher('ag-cut', 0), at_work: false, ended_reason: 'cut' as const },
  ])
  vi.mocked(updatePlatformExperiment).mockImplementation(async (_id, patch) => ({ ...pe, ...patch }) as PlatformExperiment)
})

describe('the researchers of an experiment', () => {
  it('shows the pool by type name with how many of each are at work, a stage cut not counted', async () => {
    show()
    expect(await screen.findByText('3 at work of 4 max')).toBeTruthy()
    const rows = within(screen.getByRole('table', { name: 'Pool' })).getAllByRole('row').slice(1).map(r => r.textContent)
    expect(rows).toEqual(['Generalistm22', 'LR sweeperm11'])
  })

  it('counts what the coordinator added beyond the pool on its own row', async () => {
    show({ ...pe, researchers_at_work: { ...pe.researchers_at_work!, total: 5, outside_pool: 2 } })
    expect(await screen.findByText('5 at work of 4 max')).toBeTruthy()
    const rows = within(screen.getByRole('table', { name: 'Pool' })).getAllByRole('row').slice(1).map(r => r.textContent)
    expect(rows).toEqual(['Generalistm22', 'LR sweeperm11', 'Added by the coordinator beyond the pool2'])
  })

  it('confirms a lowered max naming how many and which researchers, the newest, are cut', async () => {
    show()
    await userEvent.click(await screen.findByRole('button', { name: 'Edit' }))
    const max = screen.getByRole('spinbutton', { name: 'Max researchers' })
    await userEvent.clear(max)
    await userEvent.type(max, '1')
    expect(screen.getByText('Saving cuts the 2 newest researchers at work.')).toBeTruthy()
    await userEvent.click(screen.getByRole('button', { name: 'Save' }))
    const dialog = within(screen.getByRole('dialog'))
    expect(dialog.getByText('Lower max researchers to 1?')).toBeTruthy()
    expect(dialog.getAllByRole('listitem').map(li => li.firstChild?.textContent)).toEqual(['ag-mid', 'ag-lr'])
    expect(dialog.getByText(/^LR sweeper · joined/)).toBeTruthy()
    expect(updatePlatformExperiment).not.toHaveBeenCalled()
    await userEvent.click(dialog.getByRole('button', { name: 'Lower and cut 2' }))
    expect(updatePlatformExperiment).toHaveBeenCalledExactlyOnceWith('pe-1', { max_agents: 1 })
  })

  it('raises max and adds to the pool in one change, without asking', async () => {
    show()
    await userEvent.click(await screen.findByRole('button', { name: 'Edit' }))
    const max = screen.getByRole('spinbutton', { name: 'Max researchers' })
    await userEvent.clear(max)
    await userEvent.type(max, '6')
    await userEvent.click(screen.getByRole('button', { name: '+ Add entry' }))
    await choose(screen.getByLabelText('Pool entry 3 type'), 'Data search')
    await userEvent.click(screen.getByRole('button', { name: 'Save' }))
    await waitFor(() => expect(updatePlatformExperiment).toHaveBeenCalledExactlyOnceWith('pe-1', {
      max_agents: 6,
      researchers: [{ agent_type: 'generalist', model: 'm', count: 2 }, { agent_type: 'at-lr', model: 'm', count: 1 }, { agent_type: 'data-search', count: 1 }],
    }))
    expect(screen.queryByRole('dialog')).toBeNull()
  })

  it('refuses a pool entry below its researchers at work before the API does', async () => {
    show()
    await userEvent.click(await screen.findByRole('button', { name: 'Edit' }))
    const count = screen.getByRole('spinbutton', { name: 'Pool entry 1 count' })
    await userEvent.clear(count)
    await userEvent.type(count, '1')
    expect(screen.getByText('2 Generalist researchers on m are at work: keep at least 2. Lower max researchers to cut the newest.')).toBeTruthy()
    expect((screen.getByRole('button', { name: 'Save' }) as HTMLButtonElement).disabled).toBe(true)
  })

  it('offers no edit where the API lists no researchers in editable: to a reader, or on a closed one', async () => {
    const { unmount } = show({ ...pe, editable: [] })
    await screen.findByText('3 at work of 4 max')
    expect(screen.queryByRole('button', { name: 'Edit' })).toBeNull()
    unmount()
    show({ ...pe, status: 'closed', editable: [] })
    await screen.findByText('3 at work of 4 max')
    expect(screen.queryByRole('button', { name: 'Edit' })).toBeNull()
  })
})
