import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import {
  ApiError, apiSend, createPlatformExperiment, fetchAgentsPage, fetchExperimentStats, fetchGitHubApp, fetchGitHubInstallations, fetchGitHubRepositories,
  fetchOrgClusters, fetchPlatformExperimentsPage,
} from '@/lib/api'
import { GOAL_EXAMPLES } from '@/lib/goal-examples'
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
  apiSend: vi.fn(), createPlatformExperiment: vi.fn(), fetchAgentsPage: vi.fn(), fetchExperimentStats: vi.fn(), fetchGitHubApp: vi.fn(), fetchGitHubInstallations: vi.fn(),
  fetchGitHubRepositories: vi.fn(), fetchOrgClusters: vi.fn(), fetchPlatformExperimentsPage: vi.fn(),
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
  vi.mocked(fetchGitHubApp).mockRejectedValue(new ApiError(503, 'no GitHub App', 'github_not_configured'))
  vi.mocked(fetchGitHubInstallations).mockResolvedValue({ items: [], total: 0 })
  vi.mocked(fetchGitHubRepositories).mockResolvedValue({ items: [{ repository: 'acme/widgets', installation_id: 7, private: true }], total: 1 })
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
      expect(screen.queryByText(/New experiment in/)).toBeNull()
      expect(screen.queryByPlaceholderText('e.g. Q3 Vision Benchmark')).toBeNull()
      if (existing.length === 0) expect(await screen.findByText('No experiments yet.')).toBeTruthy()
      await userEvent.type(screen.getByLabelText('What do you want to find out?'), 'Is a cosine schedule better?')
      await userEvent.click(screen.getByRole('button', { name: 'Plan it' }))
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
    expect(await screen.findByText(/^No compute is online yet\./)).toBeTruthy()
    await userEvent.type(screen.getByLabelText('What do you want to find out?'), 'goal')
    await userEvent.click(screen.getByRole('button', { name: 'Plan it' }))
    expect(await screen.findByText(/no launcher is registered/)).toBeTruthy()
    expect(navigation.push).not.toHaveBeenCalled()
  })

  it('keeps the full form behind Configure it myself: Cancel creates nothing, a refusal stays, a created experiment opens', async () => {
    show()
    await userEvent.click(screen.getByRole('button', { name: 'Configure it myself' }))
    await userEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Cancel' }))
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(createPlatformExperiment).not.toHaveBeenCalled()

    vi.mocked(createPlatformExperiment).mockRejectedValueOnce(new Error('the org has reached its limit of platform experiments'))
    await userEvent.click(screen.getByRole('button', { name: 'Configure it myself' }))
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

// The way to plan and the way to configure were a box and a grey link labelled Advanced, under
// two paragraphs on Enter, spending and GitHub permissions.
describe('the two ways to a new experiment', () => {
  const way = (name: string) => screen.getByRole('button', { name })

  it('offers both side by side, planning pressed, and the form pressed while it is open', async () => {
    show()
    expect(within(screen.getByRole('group', { name: 'How to start' })).getAllByRole('button').map(b => b.getAttribute('aria-pressed'))).toEqual(['true', 'false'])
    expect(way('Plan it for me').getAttribute('aria-pressed')).toBe('true')
    expect(screen.queryByText(/Advanced/)).toBeNull()
    await userEvent.click(way('Configure it myself'))
    expect(screen.getByRole('dialog')).toBeTruthy()
    expect(way('Configure it myself').getAttribute('aria-pressed')).toBe('true')
  })

  it('takes the person to the goal box from Plan it for me', async () => {
    show()
    await userEvent.click(way('Plan it for me'))
    expect(document.activeElement).toBe(screen.getByLabelText('What do you want to find out?'))
  })

  it('plans from a button under the box, which waits for a sentence, with no label or line of its own', async () => {
    vi.mocked(apiSend).mockResolvedValue(experiment('pe-draft'))
    show()
    expect(way('Plan it')).toHaveProperty('disabled', true)
    expect(screen.queryByText(/until you approve|Shift\+Enter/)).toBeNull()
    expect(screen.getByLabelText('What do you want to find out?').getAttribute('placeholder')).toBe('What do you want to find out?')
    expect(screen.queryByText('What do you want to find out?')).toBeNull()
    await userEvent.type(screen.getByLabelText('What do you want to find out?'), 'Is a cosine schedule better?')
    await userEvent.click(way('Plan it'))
    expect(apiSend).toHaveBeenCalledExactlyOnceWith('POST', '/platform-experiments/drafts', { project_id: 'prj_1', goal: 'Is a cosine schedule better?' })
  })

  it('wraps a long goal in the box and never scrolls it sideways', () => {
    show()
    const box = screen.getByLabelText('What do you want to find out?')
    expect(box.className).toContain('overflow-x-hidden')
    expect(box.className).toContain('break-words')
    expect(box.className).not.toMatch(/overflow-x-(auto|scroll)|whitespace-nowrap/)
  })

  it('says in one line that no compute is online, and nothing of compute while a cluster is', async () => {
    const offline = show()
    const line = await screen.findByText(/^No compute is online yet\./)
    expect(line.textContent).toBe('No compute is online yet. Compute resources')
    expect(within(line).getByRole('link', { name: 'Compute resources' }).getAttribute('href')).toBe('/cluster?project=prj_1')
    offline.unmount()

    vi.mocked(fetchOrgClusters).mockResolvedValue([{ name: 'c1', enabled: true, connected: true } as never])
    show()
    await waitFor(() => expect(fetchOrgClusters).toHaveBeenCalledTimes(2))
    await screen.findByRole('button', { name: 'Plan it' })
    expect(screen.queryByText(/No compute is online/)).toBeNull()
  })
})

// The box gave one fixed example in its placeholder, and nothing that followed what was typed.
describe('suggested goals', () => {
  const suggestions = () => screen.queryByRole('list', { name: 'Suggestions' }) ? within(screen.getByRole('list', { name: 'Suggestions' })).getAllByRole('button').map(b => b.textContent) : []
  const box = () => screen.getByLabelText('What do you want to find out?')

  it('shows three to start from in an empty box', () => {
    show()
    expect(suggestions()).toEqual(GOAL_EXAMPLES.slice(0, 3))
  })

  it('follows the words typed so far, a word still being typed included, three at most', async () => {
    show()
    await userEvent.type(box(), 'SOTA lid')
    expect(suggestions()).toEqual(['Beat the SOTA bits-per-point on lidar point cloud compression for KITTI'])
    await userEvent.clear(box())
    await userEvent.type(box(), 'sota')
    expect(suggestions()).toHaveLength(3)
    expect(suggestions().every(s => s!.includes('SOTA'))).toBe(true)
  })

  it('shows none when nothing matches', async () => {
    show()
    await userEvent.type(box(), 'Why does my sourdough not rise')
    expect(screen.queryByRole('list', { name: 'Suggestions' })).toBeNull()
  })

  it('fills the box with the one clicked, and plans it as typed', async () => {
    vi.mocked(apiSend).mockResolvedValue(experiment('pe-draft'))
    show()
    await userEvent.type(box(), 'muon')
    const muon = 'Does Muon beat AdamW at equal compute for a 125M-parameter GPT on FineWeb?'
    await userEvent.click(screen.getByRole('button', { name: muon }))
    expect(box()).toHaveProperty('value', muon)
    expect(document.activeElement).toBe(box())
    expect(screen.queryByRole('button', { name: muon })).toBeNull()
    await userEvent.click(screen.getByRole('button', { name: 'Plan it' }))
    expect(apiSend).toHaveBeenCalledExactlyOnceWith('POST', '/platform-experiments/drafts', { project_id: 'prj_1', goal: muon })
  })
})

// The page explained GitHub permissions in a paragraph and named no repository to start from.
describe('the repository of a planned experiment', () => {
  const app = { slug: 'hl', client_id: 'c', install_url: 'https://github.test/install', authorize_url: 'https://github.test/authorize', new_repository_url: 'https://github.test/new' }

  it('leads to Settings in one line while GitHub is not connected, and the button says why it waits', async () => {
    vi.mocked(fetchGitHubApp).mockResolvedValue(app)
    show()
    const connect = await screen.findByRole('link', { name: 'Connect GitHub in Settings' })
    expect(connect.getAttribute('href')).toBe('/settings?project=prj_1#github')
    expect(connect.parentElement!.textContent).toBe('Connect GitHub in Settings to start.')
    await userEvent.type(screen.getByLabelText('What do you want to find out?'), 'Is a cosine schedule better?')
    const plan = screen.getByRole('button', { name: 'Plan it' })
    expect(plan).toHaveProperty('disabled', true)
    expect(plan.closest('[title]')!.getAttribute('title')).toBe('Connect GitHub in Settings to start')
    expect(screen.queryByText(/permission|select one only/i)).toBeNull()
    await userEvent.type(screen.getByLabelText('What do you want to find out?'), '{Enter}')
    expect(apiSend).not.toHaveBeenCalled()
  })

  it('defaults to a new private repository once connected, in one control with no how-to', async () => {
    vi.mocked(fetchGitHubApp).mockResolvedValue(app)
    vi.mocked(fetchGitHubInstallations).mockResolvedValue({
      items: [{ org_id: 'org_a', installation_id: 7, account_login: 'acme', account_type: 'Organization', connected_actor: { kind: 'user', id: 'u9', name: 'Una' }, connected_at: at }], total: 1,
    })
    show()
    const field = await screen.findByLabelText('Repository')
    expect(field.getAttribute('placeholder')).toBe('New private repo')
    expect(field.className).not.toContain('font-mono')
    expect(screen.getByText('Repository').className).toContain('sr-only')
    expect(screen.queryByRole('link', { name: 'create it on GitHub' })).toBeNull()
    expect(screen.queryByRole('link', { name: 'Connect GitHub in Settings' })).toBeNull()
  })
})
