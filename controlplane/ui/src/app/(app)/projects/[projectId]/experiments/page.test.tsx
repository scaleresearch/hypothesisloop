import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { fetchAgentsPage } from '@/lib/api/agents'
import { fetchOrgClusters } from '@/lib/api/clusters'
import { createExperimentDraft, deleteExperimentDraft, fetchExperimentDraft, fetchExperimentDrafts } from '@/lib/api/drafts'
import { beginGitHubInstall, fetchGitHubApp, fetchGitHubInstallations, fetchGitHubRepositories } from '@/lib/api/github'
import { fetchExperimentStats } from '@/lib/api/jobs'
import { checkRepository, createPlatformExperiment, fetchPlatformExperimentsPage } from '@/lib/api/platform-experiments'
import { ApiError, apiSend } from '@/lib/api/request'
import { takeReturn } from '@/lib/github-connect'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import PlatformExperimentsPage from '@/app/(app)/projects/[projectId]/experiments/page'
import type { ExperimentDraft, Me, OrgGitHubInstallation, PlatformExperiment } from '@/types/api'

const navigation = vi.hoisted(() => ({ push: vi.fn(), replace: vi.fn(), search: '' }))

vi.mock('next/navigation', () => ({
  useRouter: () => ({ push: navigation.push, replace: navigation.replace }),
  usePathname: () => '/projects/prj_1/experiments',
  useSearchParams: () => new URLSearchParams(navigation.search),
}))
vi.mock('@/lib/api/agents', async importActual => ({
  ...(await importActual<typeof import('@/lib/api/agents')>()),
  addPlatformExperimentAgents: vi.fn(), fetchAgentsPage: vi.fn(),
}))
vi.mock('@/lib/api/clusters', async importActual => ({
  ...(await importActual<typeof import('@/lib/api/clusters')>()),
  fetchOrgClusters: vi.fn(),
}))
vi.mock('@/lib/api/drafts', async importActual => ({
  ...(await importActual<typeof import('@/lib/api/drafts')>()),
  createExperimentDraft: vi.fn(), deleteExperimentDraft: vi.fn(), fetchExperimentDraft: vi.fn(), fetchExperimentDrafts: vi.fn(),
}))
vi.mock('@/lib/api/github', async importActual => ({
  ...(await importActual<typeof import('@/lib/api/github')>()),
  beginGitHubInstall: vi.fn(), fetchGitHubApp: vi.fn(), fetchGitHubInstallations: vi.fn(), fetchGitHubRepositories: vi.fn(),
}))
vi.mock('@/lib/api/jobs', async importActual => ({
  ...(await importActual<typeof import('@/lib/api/jobs')>()),
  fetchExperimentStats: vi.fn(),
}))
vi.mock('@/lib/api/platform-experiments', async importActual => ({
  ...(await importActual<typeof import('@/lib/api/platform-experiments')>()),
  checkRepository: vi.fn(), createPlatformExperiment: vi.fn(), fetchPlatformExperimentsPage: vi.fn(),
}))
vi.mock('@/lib/api/request', async importActual => ({
  ...(await importActual<typeof import('@/lib/api/request')>()),
  apiSend: vi.fn(),
}))
vi.mock('@/components/platform-experiment-card', () => ({
  PlatformExperimentCard: ({ pe }: { pe: PlatformExperiment }) => <article>{pe.name}{pe.shared ? ' (shared)' : ''}</article>,
}))

const at = '2026-01-01T00:00:00Z'
const me: Me = {
  kind: 'user', user_id: 'u1', name: 'Ada',
  orgs: [{ id: 'org_a', name: 'Lab', role: 'member', created_at: at, actions: ['create_api_key'] }],
  projects: [{ id: 'prj_1', org_id: 'org_a', name: 'default', created_at: at }],
}
const owner: Me = { ...me, orgs: [{ ...me.orgs[0], role: 'owner', actions: ['edit', 'create_api_key'] }] }
const app = { slug: 'hl', client_id: 'c', install_url: 'https://github.test/install', authorize_url: 'https://github.test/authorize', new_repository_url: 'https://github.test/new' }
const account: OrgGitHubInstallation = { org_id: 'org_a', installation_id: 7, account_login: 'acme', account_type: 'Organization', connected_actor: { kind: 'user', id: 'u9', name: 'Una' }, connected_at: '2026-01-01T00:00:00Z', default: false }
const experiment = (id: string, more: Partial<PlatformExperiment> = {}) => ({ id, project_id: 'prj_1', name: id, shared: false, ...more }) as PlatformExperiment

let own: PlatformExperiment[]
let shared: PlatformExperiment[]

function show(as: Me = me) {
  const resolved = resolveTenant(as, '')
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
  vi.mocked(fetchGitHubApp).mockReset().mockResolvedValue(app)
  vi.mocked(fetchGitHubInstallations).mockReset().mockResolvedValue({ items: [account], total: 1 })
  vi.mocked(checkRepository).mockResolvedValue({ repository_account: 'acme' })
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
      if (existing.length === 0) expect(await screen.findByText('Start your first experiment')).toBeTruthy()
      await userEvent.type(screen.getByLabelText('Describe the goal of your experiment'), 'Is a cosine schedule better?')
      await userEvent.click(screen.getByRole('button', { name: 'Plan experiment' }))
      expect(apiSend).toHaveBeenCalledExactlyOnceWith('POST', '/platform-experiments/drafts', { project_id: 'prj_1', goal: 'Is a cosine schedule better?' })
      await waitFor(() => expect(navigation.push).toHaveBeenCalledExactlyOnceWith('/experiments/pe-draft'))
      expect(createPlatformExperiment).not.toHaveBeenCalled()
      unmount()
    }
  })

  it('plans on Enter as the button does, keeps Shift+Enter for a new line, and plans nothing from an empty box', async () => {
    vi.mocked(apiSend).mockReset().mockResolvedValue(experiment('pe-draft'))
    show()
    const goal = screen.getByLabelText('Describe the goal of your experiment')
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

  it('says beside the box that a goal is needed, keeps the button pressable, and clears the message on typing', async () => {
    vi.mocked(apiSend).mockReset()
    show()
    const plan = screen.getByRole('button', { name: 'Plan experiment' })
    expect(plan).toHaveProperty('disabled', false)
    await userEvent.click(plan)
    const goal = screen.getByLabelText('Describe the goal of your experiment')
    expect(screen.getByText('Describe what the experiment should achieve.')).toBeTruthy()
    expect(goal.getAttribute('aria-invalid')).toBe('true')
    expect(document.activeElement).toBe(goal)
    expect(apiSend).not.toHaveBeenCalled()
    await userEvent.type(goal, 'D')
    expect(screen.queryByText('Describe what the experiment should achieve.')).toBeNull()
    expect(goal.getAttribute('aria-invalid')).toBeNull()
  })

  it('says why a sentence was refused, and that no cluster is online, and stays', async () => {
    vi.mocked(apiSend).mockRejectedValue(new Error('agentsandbox_not_configured: this deployment has no sandbox provider to run agent sandboxes on'))
    show()
    expect(await screen.findByText(/^No compute is online yet\./)).toBeTruthy()
    await userEvent.type(screen.getByLabelText('Describe the goal of your experiment'), 'goal')
    await userEvent.click(screen.getByRole('button', { name: 'Plan experiment' }))
    expect(await screen.findByText(/no sandbox provider to run agent sandboxes on/)).toBeTruthy()
    expect(navigation.push).not.toHaveBeenCalled()
  })

  it('keeps the full form behind Set up manually: Cancel creates nothing, a refusal stays, a created experiment opens', async () => {
    show()
    await userEvent.click(screen.getByRole('button', { name: 'Set up manually' }))
    await userEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Cancel' }))
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(createPlatformExperiment).not.toHaveBeenCalled()

    vi.mocked(createPlatformExperiment).mockRejectedValueOnce(new Error('the org has reached its limit of platform experiments'))
    await userEvent.click(screen.getByRole('button', { name: 'Set up manually' }))
    const dialog = within(screen.getByRole('dialog'))
    await userEvent.type(dialog.getByPlaceholderText('e.g. Q3 Vision Benchmark'), 'First run')
    await userEvent.type(dialog.getByLabelText('Experiment goal'), 'The brief')
    await userEvent.click(dialog.getByRole('button', { name: /^Next: / }))
    await userEvent.click(dialog.getByRole('button', { name: /^Next: / }))
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
    expect(dialog.getByLabelText('Experiment goal')).toBeTruthy()
    expect((dialog.getByLabelText('Experiment goal') as HTMLTextAreaElement).value).toBe('The kept brief')
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
    expect(screen.getByLabelText('Describe the goal of your experiment')).toBeTruthy()
  })
})

// A new person met a repository picker, a branch and a settings link before anything was planned.
describe('the repository of a planned experiment', () => {
  const unconnected = () => vi.mocked(fetchGitHubInstallations).mockResolvedValue({ items: [], total: 0 })
  let opened: { location: { href: string }; close: () => void }
  beforeEach(() => {
    opened = { location: { href: '' }, close: vi.fn() }
    vi.spyOn(window, 'open').mockReturnValue(opened as unknown as Window)
    vi.mocked(beginGitHubInstall).mockReset().mockResolvedValue({ state: 's-1', install_url: 'https://github.test/install?state=s-1', expires_at: at })
  })

  it('is a new repository unless the person picks theirs beside Plan, and is no input on a GitHub host', async () => {
    vi.mocked(apiSend).mockReset().mockResolvedValue(experiment('pe-draft'))
    show()
    await screen.findByText('Start your first experiment')
    expect(screen.queryByRole('combobox')).toBeNull()
    const control = await screen.findByRole('button', { name: 'Repository: acme / new repository' })
    await userEvent.type(screen.getByLabelText('Describe the goal of your experiment'), 'goal')
    await userEvent.click(control)
    await userEvent.click(await screen.findByRole('combobox', { name: 'Repository' }))
    await userEvent.click(await screen.findByRole('option', { name: /acme\/widgets/ }))
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(screen.getByRole('button', { name: 'Repository: acme/widgets' })).toBeTruthy()
    await userEvent.click(screen.getByRole('button', { name: 'Plan experiment' }))
    expect(apiSend).toHaveBeenCalledExactlyOnceWith('POST', '/platform-experiments/drafts', { project_id: 'prj_1', goal: 'goal', repository: 'acme/widgets', branch: 'main' })
  })

  it('has no repository control when GitHub is not connected', async () => {
    unconnected()
    show()
    await waitFor(() => expect(fetchGitHubInstallations).toHaveBeenCalled())
    expect(screen.queryByRole('button', { name: /^Repository/ })).toBeNull()
  })

  it('points the refusal about several accounts at the control', async () => {
    vi.mocked(apiSend).mockReset().mockRejectedValue(new ApiError(422, 'several', 'repository_account_required'))
    show()
    await userEvent.type(screen.getByLabelText('Describe the goal of your experiment'), 'goal{Enter}')
    expect(await screen.findByText('Several GitHub accounts are connected. Choose where the repository goes.')).toBeTruthy()
    await userEvent.click(screen.getByRole('button', { name: 'Choose' }))
    expect(await screen.findByRole('dialog', { name: 'Repository' })).toBeTruthy()
  })

  it('is not an input of the box on a GitHub host', async () => {
    show()
    await screen.findByText('Start your first experiment')
    expect(screen.queryByRole('textbox', { name: 'Repository' })).toBeNull()
    expect(screen.queryByLabelText('Branch')).toBeNull()
    expect(screen.queryByRole('link', { name: /settings/i })).toBeNull()
  })

  it('asks an owner to connect GitHub in a dialog when none is connected, and plans the typed goal once it is', async () => {
    unconnected()
    vi.mocked(apiSend).mockReset().mockResolvedValue(experiment('pe-draft'))
    show(owner)
    await userEvent.type(screen.getByLabelText('Describe the goal of your experiment'), 'Is a cosine schedule better?')
    await waitFor(() => expect(fetchGitHubInstallations).toHaveBeenCalled())
    await userEvent.click(screen.getByRole('button', { name: 'Plan experiment' }))
    const dialog = within(await screen.findByRole('dialog', { name: 'Connect GitHub' }))
    expect(dialog.getByText('Your experiment gets its own private repository. Connect GitHub so we can create it for you.')).toBeTruthy()
    expect(apiSend).not.toHaveBeenCalled()
    await userEvent.click(dialog.getByRole('button', { name: 'Connect GitHub' }))
    expect(beginGitHubInstall).toHaveBeenCalledExactlyOnceWith('org_a', '/')
    expect(opened.location.href).toBe('https://github.test/install?state=s-1')
    expect(await dialog.findByText('Finish connecting in the GitHub tab.')).toBeTruthy()

    vi.mocked(fetchGitHubInstallations).mockResolvedValue({ items: [account], total: 1 })
    await takeReturn('installation_id=7&setup_action=install&state=s-1', async () => ({ org_id: 'org_a', return_to: '/', installation: account }))
    await waitFor(() => expect(apiSend).toHaveBeenCalledWith('POST', '/platform-experiments/drafts', { project_id: 'prj_1', goal: 'Is a cosine schedule better?' }))
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
    await waitFor(() => expect(navigation.push).toHaveBeenCalledExactlyOnceWith('/experiments/pe-draft'))
  })

  it('closes the dialog on Cancel and plans nothing', async () => {
    unconnected()
    show(owner)
    await userEvent.type(screen.getByLabelText('Describe the goal of your experiment'), 'goal')
    await waitFor(() => expect(fetchGitHubInstallations).toHaveBeenCalled())
    await userEvent.click(screen.getByRole('button', { name: 'Plan experiment' }))
    await userEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Cancel' }))
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
    expect(apiSend).not.toHaveBeenCalledWith('POST', '/platform-experiments/drafts', expect.anything())
  })

  it('tells a member to ask an owner', async () => {
    unconnected()
    show()
    await userEvent.type(screen.getByLabelText('Describe the goal of your experiment'), 'goal')
    await waitFor(() => expect(fetchGitHubInstallations).toHaveBeenCalled())
    await userEvent.click(screen.getByRole('button', { name: 'Plan experiment' }))
    const dialog = within(await screen.findByRole('dialog', { name: 'Connect GitHub' }))
    expect(dialog.getByText('Ask an owner of your organization to connect GitHub.')).toBeTruthy()
    expect(dialog.queryByRole('button', { name: 'Connect GitHub' })).toBeNull()
    expect(dialog.getByRole('button', { name: 'Close' })).toBeTruthy()
  })

  it('says GitHub is no longer connected when the platform finds the account gone, with the way to connect it again', async () => {
    vi.mocked(apiSend).mockReset().mockRejectedValue(new ApiError(422, 'the GitHub App is no longer installed on acme', 'installation_gone'))
    show(owner)
    await userEvent.type(screen.getByLabelText('Describe the goal of your experiment'), 'goal{Enter}')
    expect(await screen.findByText('GitHub is no longer connected.')).toBeTruthy()
    expect(screen.queryByText(/no longer installed on acme/)).toBeNull()
    await userEvent.click(screen.getByRole('button', { name: 'Connect GitHub' }))
    expect(await screen.findByRole('dialog', { name: 'Connect GitHub' })).toBeTruthy()
  })

  it('is named in the box on a plain git host, which creates none by itself', async () => {
    vi.mocked(fetchGitHubApp).mockRejectedValue(new ApiError(503, 'no GitHub App', 'github_not_configured'))
    vi.mocked(apiSend).mockReset().mockResolvedValue(experiment('pe-draft'))
    show()
    await userEvent.type(screen.getByLabelText('Describe the goal of your experiment'), 'goal')
    const field = await screen.findByRole('textbox', { name: 'Repository' })
    await userEvent.click(screen.getByRole('button', { name: 'Plan experiment' }))
    expect(screen.getByText('Name the repository.')).toBeTruthy()
    expect(document.activeElement).toBe(field)
    expect(apiSend).not.toHaveBeenCalled()
    await userEvent.type(field, 'sweep.git{Enter}')
    expect(screen.queryByText('Name the repository.')).toBeNull()
    await userEvent.click(screen.getByRole('button', { name: 'Plan experiment' }))
    expect(apiSend).toHaveBeenCalledExactlyOnceWith('POST', '/platform-experiments/drafts', { project_id: 'prj_1', goal: 'goal', repository: 'sweep.git', branch: 'main' })
  })
})
