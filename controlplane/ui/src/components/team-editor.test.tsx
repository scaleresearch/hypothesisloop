import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useState } from 'react'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { fetchAgentTypes } from '@/lib/api/agent-types'
import { fetchModels } from '@/lib/api/models'
import { defaultTeam } from '@/lib/team'
import type { TeamRow } from '@/lib/team'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import { TeamEditor } from '@/components/team-editor'
import { CATALOG } from '@/test-agent-types'
import { MODELS } from '@/test-models'
import { choose } from '@/test-select'
import type { Me } from '@/types/api'

vi.mock('@/lib/api/agent-types', () => ({ fetchAgentTypes: vi.fn(), fetchAgentRoles: vi.fn(async () => (await import('@/test-agent-types')).ROLES) }))
vi.mock('@/lib/api/models', () => ({ fetchModels: vi.fn() }))

const at = '2026-01-01T00:00:00Z'
const me: Me = {
  kind: 'user', user_id: 'u1', name: 'Ada',
  orgs: [{ id: 'org_a', name: 'Lab', role: 'owner', created_at: at, actions: ['edit', 'create_api_key'] }],
  projects: [{ id: 'prj_1', org_id: 'org_a', name: 'default', created_at: at }],
}

let latest: TeamRow[] = []

function Editor({ initial, onModel }: { initial: TeamRow[]; onModel?: (m: string) => void }) {
  const [rows, setRows] = useState(initial)
  const [model, setModel] = useState('')
  latest = rows
  return (
    <TeamEditor
      rows={rows}
      onChange={setRows}
      coordinatorModel={model}
      coordinator={{ onModelChange: m => { setModel(m); onModel?.(m) } }}
    />
  )
}

function show(initial: TeamRow[] = defaultTeam(2), onModel?: (m: string) => void) {
  const resolved = resolveTenant(me, '')
  if (resolved.state !== 'selected') throw new Error('no tenant')
  return render(
    <SWRConfig value={{ provider: () => new Map() }}>
      <TenantContext.Provider value={resolved.tenant}><Editor initial={initial} onModel={onModel} /></TenantContext.Provider>
    </SWRConfig>,
  )
}

const entries = () => within(screen.getByRole('list', { name: 'Team' })).getAllByRole('listitem')

beforeEach(() => {
  vi.mocked(fetchAgentTypes).mockResolvedValue(CATALOG)
  vi.mocked(fetchModels).mockResolvedValue(MODELS)
})

describe('composing a team', () => {
  it('adds a second executor entry, of another type, beside the default one', async () => {
    show()
    await userEvent.click(await screen.findByRole('button', { name: '+ Add executor' }))
    expect(latest).toEqual([
      { agent_type: 'generalist', count: '2', model: '' },
      { agent_type: 'researcher', count: '1', model: '' },
      { agent_type: 'hyperparameter-search', count: '1', model: '' },
    ])
    await choose(screen.getByLabelText('Team entry 3 type'), 'Data search')
    expect(latest[2].agent_type).toBe('data-search')
    expect(screen.queryByText(/Raise its count instead/)).toBeNull()
  })

  it('raises the count where every type of the role is in the team already', async () => {
    show()
    await userEvent.click(await screen.findByRole('button', { name: '+ Add researcher' }))
    expect(latest[1]).toEqual({ agent_type: 'researcher', count: '2', model: '' })
  })

  it('flags an entry of a type and model another entry has', async () => {
    show([{ agent_type: 'generalist', count: '1', model: '' }, { agent_type: 'data-search', count: '1', model: '' }])
    await choose(await screen.findByLabelText('Team entry 2 type'), 'Generalist')
    expect(screen.getByText('Entry 1 has this type and model. Raise its count instead.')).toBeTruthy()
  })

  it('offers a way to make another agent type from the type select', async () => {
    const open = vi.spyOn(window, 'open').mockReturnValue(null)
    show()
    await choose(await screen.findByLabelText('Team entry 1 type'), 'Manage agent types')
    expect(open).toHaveBeenCalledWith('/orgs/org_a/settings/agent-types', '_blank', 'noopener')
    expect(latest[0].agent_type).toBe('generalist')
    open.mockRestore()
  })

  it('shows what an entry starts from, kept as it is, with no field to type JSON in', async () => {
    const hyperparameters = { direction: 'Tune the warmup first.' }
    show([{ agent_type: 'generalist', count: '1', model: '', hyperparameters }, { agent_type: 'data-search', count: '1', model: '', hyperparameters: { lr: 0.1 } }])
    expect(await screen.findByText('Starts from: Tune the warmup first.')).toBeTruthy()
    expect(screen.getByText('Starts from: {"lr":0.1}')).toBeTruthy()
    expect(screen.queryByRole('button', { name: '+ Hyperparameters' })).toBeNull()
    expect(screen.queryByRole('textbox', { name: /hyperparameters/ })).toBeNull()
    const count = screen.getByLabelText('Team entry 1 count')
    await userEvent.clear(count)
    await userEvent.type(count, '3')
    expect(latest[0]).toEqual({ agent_type: 'generalist', count: '3', model: '', hyperparameters })
  })

  it('shows the coordinator first, one of it, its model editable and no way to remove it', async () => {
    const onModel = vi.fn()
    show(defaultTeam(2), onModel)
    const first = (await screen.findByRole('listitem', { name: 'Coordinator' }))
    expect(entries()[0]).toBe(first)
    expect(within(first).getByLabelText('Coordinator count')).toHaveProperty('disabled', true)
    expect(within(first).getByLabelText('Coordinator count')).toHaveProperty('value', '1')
    expect(within(first).getByLabelText('Coordinator type')).toHaveProperty('disabled', true)
    expect(within(first).queryByRole('button', { name: /Remove/ })).toBeNull()
    await userEvent.click(within(first).getByLabelText('Coordinator model'))
    await userEvent.click(await screen.findByRole('option', { name: /claude-sonnet-5/ }))
    expect(onModel).toHaveBeenCalledWith('anthropic/claude-sonnet-5')
    expect((screen.getByLabelText('Team entry 2 model') as HTMLInputElement).placeholder).toBe("Coordinator's model: anthropic/claude-sonnet-5")
  })

  it('counts the executors of every executor type together', async () => {
    show([{ agent_type: 'generalist', count: '2', model: '' }, { agent_type: 'at-lr', count: '3', model: '' }, { agent_type: 'researcher', count: '1', model: '' }])
    expect(await screen.findByText('5 executors, 1 researcher')).toBeTruthy()
  })
})
