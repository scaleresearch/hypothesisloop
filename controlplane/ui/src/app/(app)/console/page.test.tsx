import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import {
  createPlatformExperiment, fetchAgentsPage, fetchExperimentStats, fetchGitHubInstallations, fetchOrgClusters, fetchPlatformExperimentsPage,
} from '@/lib/api'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import Home from '@/app/(app)/console/page'
import type { Me, PlatformExperiment } from '@/types'

const navigation = vi.hoisted(() => ({ push: vi.fn() }))

vi.mock('next/navigation', () => ({
  useRouter: () => ({ push: navigation.push, replace: vi.fn() }),
  useSearchParams: () => new URLSearchParams('project=prj_1'),
}))
vi.mock('@/lib/api', async importActual => ({
  ...(await importActual<typeof import('@/lib/api')>()),
  createPlatformExperiment: vi.fn(), fetchAgentsPage: vi.fn(), fetchExperimentStats: vi.fn(), fetchGitHubInstallations: vi.fn(),
  fetchOrgClusters: vi.fn(), fetchPlatformExperimentsPage: vi.fn(),
}))
vi.mock('@/components/platform-experiment-card', () => ({
  PlatformExperimentCard: ({ pe }: { pe: PlatformExperiment }) => <article>{pe.name}{pe.shared ? ' (shared)' : ''}</article>,
}))

const at = '2026-01-01T00:00:00Z'
const me: Me = {
  kind: 'user', user_id: 'u1', name: 'Ada',
  orgs: [{ id: 'org_a', name: 'Lab', role: 'member', created_at: at }],
  projects: [{ id: 'prj_1', org_id: 'org_a', name: 'default', created_at: at }],
}
const experiment = (id: string, more: Partial<PlatformExperiment> = {}) => ({ id, project_id: 'prj_1', name: id, shared: false, ...more }) as PlatformExperiment

let own: PlatformExperiment[]
let shared: PlatformExperiment[]

function show() {
  const resolved = resolveTenant(me, '')
  if (resolved.state !== 'selected') throw new Error('no tenant')
  return render(
    <SWRConfig value={{ provider: () => new Map() }}>
      <TenantContext.Provider value={resolved.tenant}><Home /></TenantContext.Provider>
    </SWRConfig>,
  )
}

beforeEach(() => {
  navigation.push.mockReset()
  own = []
  shared = []
  vi.mocked(fetchOrgClusters).mockResolvedValue([])
  vi.mocked(fetchExperimentStats).mockResolvedValue({ total: 0, by_status: {} } as never)
  vi.mocked(fetchAgentsPage).mockResolvedValue({ items: [], total: 0 })
  vi.mocked(fetchGitHubInstallations).mockResolvedValue({ items: [], total: 0 })
  vi.mocked(fetchPlatformExperimentsPage).mockImplementation(async params => {
    const all = params?.shared === false ? own : [...own, ...shared]
    return { items: all.slice(0, params?.limit), total: all.length }
  })
  vi.mocked(createPlatformExperiment).mockImplementation(async (_project, req) => experiment('pe-new', { name: req.name }))
})

describe('the home page of a project with no platform experiment of its own', () => {
  it('is the form to create one, with nothing to cancel, and opens the experiment once it is created', async () => {
    show()
    expect(await screen.findByText('New platform experiment in Lab / default')).toBeTruthy()
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(screen.queryByRole('button', { name: 'Cancel' })).toBeNull()
    await userEvent.type(screen.getByPlaceholderText('e.g. Q3 Vision Benchmark'), 'First run')
    await userEvent.click(screen.getByRole('button', { name: 'Next: competition settings' }))
    await userEvent.click(screen.getByRole('button', { name: 'Create Experiment' }))
    expect(createPlatformExperiment).toHaveBeenCalledTimes(1)
    expect(vi.mocked(createPlatformExperiment).mock.calls[0][0]).toBe('prj_1')
    expect(vi.mocked(createPlatformExperiment).mock.calls[0][1]).toMatchObject({ name: 'First run' })
    await waitFor(() => expect(navigation.push).toHaveBeenCalledExactlyOnceWith('/platform-experiments/pe-new?project=prj_1'))
  })

  it('shows the API\'s refusal in its words and stays on the form', async () => {
    vi.mocked(createPlatformExperiment).mockRejectedValue(new Error('the org has reached its limit of platform experiments'))
    show()
    await userEvent.type(await screen.findByPlaceholderText('e.g. Q3 Vision Benchmark'), 'First run')
    await userEvent.click(screen.getByRole('button', { name: 'Next: competition settings' }))
    await userEvent.click(screen.getByRole('button', { name: 'Create Experiment' }))
    expect(await screen.findByText('the org has reached its limit of platform experiments')).toBeTruthy()
    expect(navigation.push).not.toHaveBeenCalled()
  })

  it('lists what the platform shares below the form, which does not count as the project\'s own', async () => {
    shared = [experiment('pe-shared', { project_id: 'prj_other', shared: true })]
    show()
    expect(await screen.findByText('New platform experiment in Lab / default')).toBeTruthy()
    expect(screen.getByText('Shared by the platform')).toBeTruthy()
    expect(screen.getByText('pe-shared (shared)')).toBeTruthy()
    expect(fetchPlatformExperimentsPage).toHaveBeenCalledWith({ project_id: 'prj_1', shared: false, limit: 1 })
  })
})

describe('the home page of a project with platform experiments', () => {
  beforeEach(() => {
    own = Array.from({ length: 7 }, (_, i) => experiment(`pe-${i}`))
  })

  it('lists the newest, leads to the rest, and keeps the form behind New experiment', async () => {
    show()
    expect(await screen.findByRole('heading', { name: 'Platform experiments of default' })).toBeTruthy()
    expect(screen.getAllByRole('article').map(a => a.textContent)).toEqual(['pe-0', 'pe-1', 'pe-2', 'pe-3', 'pe-4'])
    expect(screen.getByRole('link', { name: 'All 7 platform experiments →' }).getAttribute('href')).toBe('/platform-experiments?project=prj_1')
    expect(screen.queryByPlaceholderText('e.g. Q3 Vision Benchmark')).toBeNull()
  })

  it('opens the same form in a dialog, and the experiment once it is created', async () => {
    show()
    await userEvent.click(await screen.findByRole('button', { name: 'New experiment' }))
    const dialog = within(screen.getByRole('dialog'))
    await userEvent.type(dialog.getByPlaceholderText('e.g. Q3 Vision Benchmark'), 'Another run')
    await userEvent.click(dialog.getByRole('button', { name: 'Next: competition settings' }))
    await userEvent.click(dialog.getByRole('button', { name: 'Create Experiment' }))
    await waitFor(() => expect(navigation.push).toHaveBeenCalledExactlyOnceWith('/platform-experiments/pe-new?project=prj_1'))
    expect(screen.queryByRole('dialog')).toBeNull()
  })

  it('closes the dialog on Cancel and creates nothing', async () => {
    show()
    await userEvent.click(await screen.findByRole('button', { name: 'New experiment' }))
    await userEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Cancel' }))
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(createPlatformExperiment).not.toHaveBeenCalled()
  })
})

describe('a home page that cannot read the experiments', () => {
  it('says so in the API\'s words and offers no form that might hide it', async () => {
    vi.mocked(fetchPlatformExperimentsPage).mockRejectedValue(new Error('API unreachable'))
    show()
    expect(await screen.findByText('Could not load the platform experiments of default: API unreachable')).toBeTruthy()
    expect(screen.queryByPlaceholderText('e.g. Q3 Vision Benchmark')).toBeNull()
  })
})
