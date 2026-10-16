import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import {
  ApiError, apiSend, createExperimentDraft, createPlatformExperiment, deleteExperimentDraft, fetchAgentsPage, fetchExperimentDraft, fetchExperimentDrafts,
  fetchExperimentStats, fetchGitHubApp, fetchGitHubInstallations, fetchGitHubRepositories, fetchOrgClusters, fetchPlatformExperimentsPage,
} from '@/lib/api'
import type { ExperimentDraft } from '@/lib/api'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import PlatformExperimentsPage from '@/app/(app)/projects/[projectId]/experiments/page'
import type { Me, PlatformExperiment } from '@/types'

const navigation = vi.hoisted(() => ({ push: vi.fn(), replace: vi.fn(), search: '' }))

vi.mock('next/navigation', () => ({
  useRouter: () => ({ push: navigation.push, replace: navigation.replace }),
  usePathname: () => '/projects/prj_1/experiments',
  useSearchParams: () => new URLSearchParams(navigation.search),
}))
vi.mock('@/lib/api', async importActual => ({
  ...(await importActual<typeof import('@/lib/api')>()),
  addPlatformExperimentAgents: vi.fn(), apiSend: vi.fn(), createPlatformExperiment: vi.fn(), fetchAgentsPage: vi.fn(), fetchExperimentStats: vi.fn(), fetchGitHubApp: vi.fn(), fetchGitHubInstallations: vi.fn(),
  fetchGitHubRepositories: vi.fn(), fetchOrgClusters: vi.fn(), fetchPlatformExperimentsPage: vi.fn(),
  createExperimentDraft: vi.fn(), deleteExperimentDraft: vi.fn(), fetchExperimentDraft: vi.fn(), fetchExperimentDrafts: vi.fn(),
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

const kept: ExperimentDraft = {
  id: 'draft-1', project_id: 'prj_1', step: '3', failure: 'GitHub refused to create acme/run', created_at: at, updated_at: at,
  form: { name: 'Kept run', description: 'The kept brief' },
}

beforeEach(() => {
  navigation.push.mockReset()
  navigation.replace.mockReset()
  navigation.search = ''
  vi.mocked(fetchExperimentDrafts).mockReset().mockResolvedValue([])
  vi.mocked(fetchExperimentDraft).mockReset().mockResolvedValue(kept)
  vi.mocked(createExperimentDraft).mockReset().mockImplementation(async (_, body) => ({ ...kept, ...body }))
  vi.mocked(deleteExperimentDraft).mockReset().mockResolvedValue()
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
      await waitFor(() => expect(navigation.push).toHaveBeenCalledExactlyOnceWith('/experiments/pe-draft'))
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
    await waitFor(() => expect(navigation.push).toHaveBeenCalledExactlyOnceWith('/experiments/pe-draft'))
  })

  // Planners proposed on readings of what a win is or what is held equal that the person never
  // stated, and agents then guessed the rest from the code.
  it('offers the points a goal leaves open once the box is taken, each as a line to fill in, ticked once filled', async () => {
    vi.mocked(apiSend).mockReset().mockResolvedValue(experiment('pe-draft'))
    show()
    expect(screen.queryByRole('button', { name: 'Win' })).toBeNull()
    const goal = screen.getByLabelText('What do you want to find out?')
    await userEvent.type(goal, 'Does Lion beat AdamW?')
    const win = screen.getByRole('button', { name: 'Win' })
    expect(win.getAttribute('aria-pressed')).toBe('false')
    await userEvent.click(win)
    expect(screen.getByText(/^How much better counts/)).toBeTruthy()
    expect(document.activeElement).toBe(goal)
    await userEvent.keyboard('1% lower val loss over 3 seeds')
    expect(goal).toHaveProperty('value', 'Does Lion beat AdamW?\nWin: 1% lower val loss over 3 seeds')
    expect(win.getAttribute('aria-pressed')).toBe('true')
    await userEvent.click(screen.getByRole('button', { name: 'Plan it' }))
    expect(apiSend).toHaveBeenCalledExactlyOnceWith('POST', '/platform-experiments/drafts', { project_id: 'prj_1', goal: 'Does Lion beat AdamW?\nWin: 1% lower val loss over 3 seeds' })
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
    await userEvent.type(dialog.getByLabelText('Brief'), 'The brief')
    await userEvent.click(dialog.getByRole('button', { name: 'Next' }))
    await userEvent.click(dialog.getByRole('button', { name: 'Next' }))
    await userEvent.click(dialog.getByRole('button', { name: 'Create experiment' }))
    expect(await dialog.findByText('the org has reached its limit of platform experiments')).toBeTruthy()
    expect(navigation.push).not.toHaveBeenCalled()
    await waitFor(() => expect(createExperimentDraft).toHaveBeenCalledTimes(1))
    await userEvent.click(dialog.getByRole('button', { name: 'Try again' }))
    expect(vi.mocked(createPlatformExperiment).mock.calls[1]).toMatchObject(['prj_1', { name: 'First run', description: 'The brief' }])
    await waitFor(() => expect(navigation.push).toHaveBeenCalledExactlyOnceWith('/experiments/pe-new'))
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(deleteExperimentDraft).toHaveBeenCalledExactlyOnceWith('prj_1', 'draft-1')
  })

  it('lists the saved drafts with why each was last refused, and opens one at its step', async () => {
    vi.mocked(fetchExperimentDrafts).mockResolvedValue([kept])
    show()
    expect(await screen.findByText('Kept run')).toBeTruthy()
    expect(screen.getByText('Not created: GitHub refused to create acme/run')).toBeTruthy()
    await userEvent.click(screen.getByRole('button', { name: 'Continue' }))
    const dialog = within(screen.getByRole('dialog'))
    expect(dialog.getByRole('tab', { name: '3. Budget and rules', selected: true })).toBeTruthy()
    expect(dialog.getByText('GitHub refused to create acme/run')).toBeTruthy()
    await userEvent.click(dialog.getByRole('button', { name: 'Back' }))
    await userEvent.click(dialog.getByRole('button', { name: 'Back' }))
    expect((dialog.getByPlaceholderText('e.g. Q3 Vision Benchmark') as HTMLInputElement).value).toBe('Kept run')
  })

  it('opens the draft ?draft names, as GitHub\'s return asks, and leaves the address', async () => {
    navigation.search = 'draft=draft-1'
    vi.mocked(fetchExperimentDraft).mockResolvedValue({ ...kept, step: '1' })
    show()
    const dialog = within(await screen.findByRole('dialog'))
    expect(fetchExperimentDraft).toHaveBeenCalledWith('prj_1', 'draft-1')
    expect(navigation.replace).toHaveBeenCalledWith('/projects/prj_1/experiments')
    expect(dialog.getByLabelText('Brief')).toBeTruthy()
    expect((dialog.getByLabelText('Brief') as HTMLTextAreaElement).value).toBe('The kept brief')
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

// The page explained GitHub permissions in a paragraph and named no repository to start from.
describe('the repository of a planned experiment', () => {
  const app = { slug: 'hl', client_id: 'c', install_url: 'https://github.test/install', authorize_url: 'https://github.test/authorize', new_repository_url: 'https://github.test/new' }

  it('leads to Settings in one line while GitHub is not connected, and the button says why it waits', async () => {
    vi.mocked(fetchGitHubApp).mockResolvedValue(app)
    show()
    const connect = await screen.findByRole('link', { name: 'Connect GitHub in Settings' })
    expect(connect.getAttribute('href')).toBe('/orgs/org_a/settings#github')
    expect(connect.parentElement!.textContent).toBe('Connect GitHub in Settings to start.')
    await userEvent.type(screen.getByLabelText('What do you want to find out?'), 'Is a cosine schedule better?')
    const plan = screen.getByRole('button', { name: 'Plan it' })
    expect(plan).toHaveProperty('disabled', true)
    expect(plan.closest('[title]')!.getAttribute('title')).toBe('Connect GitHub in Settings to start')
    expect(screen.queryByText(/permission|select one only/i)).toBeNull()
    await userEvent.type(screen.getByLabelText('What do you want to find out?'), '{Enter}')
    expect(apiSend).not.toHaveBeenCalled()
  })
})
