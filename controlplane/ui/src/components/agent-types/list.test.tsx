import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createAgentType, fetchAgentTypes } from '@/lib/api/agent-types'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import { AgentTypesList } from '@/components/agent-types/list'
import { CATALOG, agentType } from '@/test-agent-types'
import type { Me } from '@/types/api'

const navigation = vi.hoisted(() => ({ push: vi.fn() }))
vi.mock('next/navigation', () => ({ useRouter: () => navigation }))
vi.mock('@/lib/api/agent-types', () => ({ fetchAgentTypes: vi.fn(), createAgentType: vi.fn() }))

const at = '2026-01-01T00:00:00Z'
const me = (role: 'owner' | 'member'): Me => ({
  kind: 'user', user_id: 'u1', name: 'Ada',
  orgs: [{ id: 'org_a', name: 'Lab', role, created_at: at }],
  projects: [{ id: 'prj_1', org_id: 'org_a', name: 'default', created_at: at }],
})

function show(role: 'owner' | 'member' = 'owner') {
  const resolved = resolveTenant(me(role), '')
  if (resolved.state !== 'selected') throw new Error('no tenant')
  return render(
    <SWRConfig value={{ provider: () => new Map() }}>
      <TenantContext.Provider value={resolved.tenant}><AgentTypesList /></TenantContext.Provider>
    </SWRConfig>,
  )
}

beforeEach(() => {
  navigation.push.mockReset()
  vi.mocked(fetchAgentTypes).mockResolvedValue([...CATALOG, agentType('at-old', 'Old sweeper', 'researcher', { archived_at: at })])
  vi.mocked(createAgentType).mockResolvedValue(agentType('at-new', 'Copy', 'researcher'))
})

describe('the agent types page', () => {
  it('groups types by role, built-in first marked read-only, archived last', async () => {
    show()
    const researchers = within(await screen.findByRole('table', { name: 'Researcher types' }))
    const names = researchers.getAllByRole('link').map(l => l.textContent)
    expect(names).toEqual(['Generalist', 'Hyperparameter search', 'Architecture search', 'Data search', 'LR sweeper', 'Old sweeper'])
    expect(researchers.getAllByText('Built-in, read-only')).toHaveLength(4)
    expect(researchers.getByText('Archived')).toBeTruthy()
    expect(researchers.queryByRole('columnheader', { name: 'In use' })).toBeNull()
    for (const role of ['Baseline', 'Planner', 'Coordinator']) expect(screen.getByRole('table', { name: `${role} types` })).toBeTruthy()
  })

  it('duplicates a researcher or baseline type, built-in ones included, and opens the copy', async () => {
    show()
    const row = (await screen.findByRole('link', { name: 'Data search' })).closest('tr')!
    await userEvent.click(within(row).getByRole('button', { name: 'Duplicate' }))
    expect(createAgentType).toHaveBeenCalledExactlyOnceWith('org_a', { from: 'data-search' })
    expect(navigation.push).toHaveBeenCalledWith('/orgs/org_a/settings/agent-types/at-new')
    for (const role of ['Planner', 'Coordinator']) {
      expect(within(screen.getByRole('table', { name: `${role} types` })).queryByRole('button', { name: 'Duplicate' })).toBeNull()
    }
  })

  it('starts a new type from the built-in one of the role chosen, a researcher or a baseline', async () => {
    show()
    await userEvent.click(await screen.findByRole('button', { name: 'New type' }))
    const dialog = within(screen.getByRole('dialog'))
    expect(dialog.queryByRole('button', { name: 'Coordinator' })).toBeNull()
    expect(dialog.queryByRole('button', { name: 'Planner' })).toBeNull()
    await userEvent.click(dialog.getByRole('button', { name: 'Baseline' }))
    expect(dialog.getByText('Starts as a copy of Baseline.')).toBeTruthy()
    await userEvent.type(dialog.getByLabelText('Name'), 'Strict baseline')
    await userEvent.click(dialog.getByRole('button', { name: 'Create and edit' }))
    expect(createAgentType).toHaveBeenCalledExactlyOnceWith('org_a', { from: 'baseline', name: 'Strict baseline' })
  })

  it('shows the API\'s refusal of a copy', async () => {
    vi.mocked(createAgentType).mockRejectedValue(new Error('only an owner makes agent types'))
    show()
    const row = (await screen.findByRole('link', { name: 'Generalist' })).closest('tr')!
    await userEvent.click(within(row).getByRole('button', { name: 'Duplicate' }))
    expect(await screen.findByText('only an owner makes agent types')).toBeTruthy()
    expect(navigation.push).not.toHaveBeenCalled()
  })

  it('lets a member read every type and says only an owner changes them', async () => {
    show('member')
    await screen.findByRole('table', { name: 'Researcher types' })
    expect(screen.getByRole('note').textContent).toMatch(/Only an owner of Lab can create, edit or archive agent types/)
    expect(screen.queryByRole('button', { name: 'Duplicate' })).toBeNull()
    expect(screen.queryByRole('button', { name: 'New type' })).toBeNull()
  })
})
