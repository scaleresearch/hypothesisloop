import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import {
  apiSend, createPlatformExperiment, fetchAgentsPage, fetchExperimentStats, fetchGitHubInstallations, fetchOrgClusters, fetchPlatformExperimentsPage,
} from '@/lib/api'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import PlatformExperimentsPage from '@/app/(app)/platform-experiments/page'
import type { Me, PlatformExperiment } from '@/types'

const navigation = vi.hoisted(() => ({ push: vi.fn() }))

vi.mock('next/navigation', () => ({
  useRouter: () => ({ push: navigation.push, replace: vi.fn() }),
  usePathname: () => '/platform-experiments',
  useSearchParams: () => new URLSearchParams('project=prj_1'),
}))
vi.mock('@/lib/api', async importActual => ({
  ...(await importActual<typeof import('@/lib/api')>()),
  apiSend: vi.fn(), createPlatformExperiment: vi.fn(), fetchAgentsPage: vi.fn(), fetchExperimentStats: vi.fn(), fetchGitHubInstallations: vi.fn(),
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
      <TenantContext.Provider value={resolved.tenant}><PlatformExperimentsPage /></TenantContext.Provider>
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

// The form asks for metrics, a budget and dates before anything exists, so the page starts
// from a sentence and keeps the form for who wants to state all of that themselves.
describe('the experiments page', () => {
  it('starts an experiment from one sentence, in an empty project and in one with experiments', async () => {
    vi.mocked(apiSend).mockResolvedValue(experiment('pe-draft'))
    for (const existing of [[], [experiment('pe-0')]]) {
      own = existing
      navigation.push.mockReset()
      vi.mocked(apiSend).mockClear()
      const { unmount } = show()
      expect(screen.getByText('New experiment in Lab / default')).toBeTruthy()
      expect(screen.queryByPlaceholderText('e.g. Q3 Vision Benchmark')).toBeNull()
      if (existing.length === 0) expect(await screen.findByText(/None yet\. Write what you want to find out above/)).toBeTruthy()
      await userEvent.type(screen.getByLabelText('What do you want to find out?'), 'Is a cosine schedule better?')
      await userEvent.click(screen.getByRole('button', { name: 'Plan this experiment' }))
      expect(apiSend).toHaveBeenCalledExactlyOnceWith('POST', '/platform-experiments/drafts', { project_id: 'prj_1', goal: 'Is a cosine schedule better?' })
      await waitFor(() => expect(navigation.push).toHaveBeenCalledExactlyOnceWith('/platform-experiments/pe-draft?project=prj_1'))
      expect(createPlatformExperiment).not.toHaveBeenCalled()
      unmount()
    }
  })

  it('plans on Enter as the button does, keeps Shift+Enter for a new line, and plans nothing from an empty box', async () => {
    vi.mocked(apiSend).mockReset().mockResolvedValue(experiment('pe-draft'))
    show()
    const goal = screen.getByLabelText('What do you want to find out?')
    await userEvent.type(goal, '  {Enter}')
    expect(apiSend).not.toHaveBeenCalled()
    await userEvent.clear(goal)
    await userEvent.type(goal, 'Is a cosine schedule better?{Shift>}{Enter}{/Shift}On small models.')
    expect(apiSend).not.toHaveBeenCalled()
    expect(goal).toHaveProperty('value', 'Is a cosine schedule better?\nOn small models.')
    await userEvent.type(goal, '{Enter}')
    expect(apiSend).toHaveBeenCalledExactlyOnceWith('POST', '/platform-experiments/drafts', { project_id: 'prj_1', goal: 'Is a cosine schedule better?\nOn small models.' })
    await waitFor(() => expect(navigation.push).toHaveBeenCalledExactlyOnceWith('/platform-experiments/pe-draft?project=prj_1'))
  })

  it('says why a sentence was refused, and that no cluster is online, and stays', async () => {
    vi.mocked(apiSend).mockRejectedValue(new Error('no launcher is registered, so nothing would run the planner'))
    show()
    expect(await screen.findByText(/No cluster this org can use is online/)).toBeTruthy()
    await userEvent.type(screen.getByLabelText('What do you want to find out?'), 'goal')
    await userEvent.click(screen.getByRole('button', { name: 'Plan this experiment' }))
    expect(await screen.findByText(/no launcher is registered/)).toBeTruthy()
    expect(navigation.push).not.toHaveBeenCalled()
  })

  it('keeps the full form behind Advanced: Cancel creates nothing, a refusal stays, a created experiment opens', async () => {
    show()
    await userEvent.click(screen.getByRole('button', { name: 'Advanced' }))
    await userEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Cancel' }))
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(createPlatformExperiment).not.toHaveBeenCalled()

    vi.mocked(createPlatformExperiment).mockRejectedValueOnce(new Error('the org has reached its limit of platform experiments'))
    await userEvent.click(screen.getByRole('button', { name: 'Advanced' }))
    const dialog = within(screen.getByRole('dialog'))
    await userEvent.type(dialog.getByPlaceholderText('e.g. Q3 Vision Benchmark'), 'First run')
    await userEvent.type(dialog.getByPlaceholderText(/^Goals, evaluation protocol/), 'The brief')
    await userEvent.click(dialog.getByRole('button', { name: 'Next: budget and rules' }))
    await userEvent.click(dialog.getByRole('button', { name: 'Create experiment' }))
    expect(await dialog.findByText('the org has reached its limit of platform experiments')).toBeTruthy()
    expect(navigation.push).not.toHaveBeenCalled()
    await userEvent.click(dialog.getByRole('button', { name: 'Create experiment' }))
    expect(vi.mocked(createPlatformExperiment).mock.calls[1]).toMatchObject(['prj_1', { name: 'First run', description: 'The brief' }])
    await waitFor(() => expect(navigation.push).toHaveBeenCalledExactlyOnceWith('/platform-experiments/pe-new?project=prj_1'))
    expect(screen.queryByRole('dialog')).toBeNull()
  })

  it('lists the experiments under the goal box, or says it could not read them and keeps the box', async () => {
    own = [experiment('pe-0'), experiment('pe-1')]
    shared = [experiment('pe-shared', { project_id: 'prj_other', shared: true })]
    const { unmount } = show()
    expect(await screen.findByRole('heading', { name: 'Experiments' })).toBeTruthy()
    await screen.findByText('pe-0')
    expect(screen.getAllByRole('article').map(a => a.textContent)).toEqual(['pe-0', 'pe-1', 'pe-shared (shared)'])
    unmount()

    vi.mocked(fetchPlatformExperimentsPage).mockRejectedValue(new Error('API unreachable'))
    show()
    expect(await screen.findByText('Could not load the experiments of default: API unreachable')).toBeTruthy()
    expect(screen.getByLabelText('What do you want to find out?')).toBeTruthy()
  })
})
