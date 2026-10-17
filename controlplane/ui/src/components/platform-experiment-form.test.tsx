import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { fetchAgentTypes } from '@/lib/api/agent-types'
import { addPlatformExperimentAgents } from '@/lib/api/agents'
import { createExperimentDraft, deleteExperimentDraft, updateExperimentDraft } from '@/lib/api/drafts'
import { beginGitHubInstall, fetchGitHubApp, fetchGitHubInstallations, fetchGitHubRepositories } from '@/lib/api/github'
import { createPlatformExperiment, fetchExperimentFile, updatePlatformExperiment } from '@/lib/api/platform-experiments'
import { ApiError } from '@/lib/api/request'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import { PlatformExperimentForm } from '@/components/platform-experiment-form'
import { CATALOG } from '@/test-agent-types'
import { choose } from '@/test-select'
import type { ExperimentDraft, Me, PlatformExperiment } from '@/types/api'

vi.mock('@/lib/api/agent-types', async importActual => ({
  ...(await importActual<typeof import('@/lib/api/agent-types')>()),
  fetchAgentTypes: vi.fn(),
}))
vi.mock('@/lib/api/agents', async importActual => ({
  ...(await importActual<typeof import('@/lib/api/agents')>()),
  addPlatformExperimentAgents: vi.fn(),
}))
vi.mock('@/lib/api/drafts', async importActual => ({
  ...(await importActual<typeof import('@/lib/api/drafts')>()),
  createExperimentDraft: vi.fn(), updateExperimentDraft: vi.fn(), deleteExperimentDraft: vi.fn(),
}))
vi.mock('@/lib/api/github', async importActual => ({
  ...(await importActual<typeof import('@/lib/api/github')>()),
  beginGitHubInstall: vi.fn(), fetchGitHubApp: vi.fn(), fetchGitHubInstallations: vi.fn(), fetchGitHubRepositories: vi.fn(),
}))
vi.mock('@/lib/api/platform-experiments', async importActual => ({
  ...(await importActual<typeof import('@/lib/api/platform-experiments')>()),
  createPlatformExperiment: vi.fn(), fetchExperimentFile: vi.fn(), updatePlatformExperiment: vi.fn(),
}))

const at = '2026-01-01T00:00:00Z'
const me: Me = {
  kind: 'user', user_id: 'u1', name: 'Ada',
  orgs: [{ id: 'org_a', name: 'Lab', role: 'owner', created_at: at, actions: ['edit', 'create_api_key'] }],
  projects: [{ id: 'prj_1', org_id: 'org_a', name: 'default', created_at: at }],
}
const created = { id: 'pe-new', project_id: 'prj_1', name: 'Run' } as PlatformExperiment
const onSaved = vi.fn()

function show(initial?: PlatformExperiment, draft?: ExperimentDraft) {
  const resolved = resolveTenant(me, '')
  if (resolved.state !== 'selected') throw new Error('no tenant')
  return render(
    <SWRConfig value={{ provider: () => new Map() }}>
      <TenantContext.Provider value={resolved.tenant}><PlatformExperimentForm initial={initial} draft={draft} onSaved={onSaved} /></TenantContext.Provider>
    </SWRConfig>,
  )
}

const name = () => screen.getByPlaceholderText('e.g. Q3 Vision Benchmark')
const next = () => userEvent.click(screen.getByRole('button', { name: 'Next' }))
const create = () => userEvent.click(screen.getByRole('button', { name: 'Create experiment' }))
const sent = (call = 0) => vi.mocked(createPlatformExperiment).mock.calls[call][1]

async function chooseRepository(repository: string) {
  await userEvent.click(await screen.findByLabelText('Repository'))
  await userEvent.click(await screen.findByRole('option', { name: repository }))
}

// The name and the brief, then on through the steps to the last.
async function essentials({ repository }: { repository?: string } = {}) {
  await userEvent.type(name(), 'Run')
  await userEvent.type(screen.getByLabelText('Brief'), 'The brief')
  if (repository) await chooseRepository(repository)
  await next()
  await next()
}

beforeEach(() => {
  onSaved.mockReset()
  vi.mocked(fetchExperimentFile).mockReset().mockRejectedValue(new ApiError(422, 'the repository has no experiment file', 'no_experiment_file'))
  vi.mocked(fetchGitHubApp).mockResolvedValue({ slug: 'hl', install_url: 'https://github.test/install', new_repository_url: 'https://github.test/new' })
  vi.mocked(fetchGitHubInstallations).mockResolvedValue({ items: [{ org_id: 'org_a', installation_id: 7, account_login: 'acme', connected_actor: { kind: 'user', id: 'u9', name: 'Una' }, connected_at: at, default: false }], total: 1 })
  vi.mocked(fetchGitHubRepositories).mockResolvedValue({ items: [{ repository: 'acme/widgets', installation_id: 7, private: false }], total: 1 })
  vi.mocked(createPlatformExperiment).mockReset().mockResolvedValue(created)
  vi.mocked(addPlatformExperimentAgents).mockReset().mockResolvedValue([])
  vi.mocked(updatePlatformExperiment).mockReset()
  const kept = (id: string) => async (_: string, ...rest: unknown[]) => {
    const body = rest[rest.length - 1] as Pick<ExperimentDraft, 'form' | 'step' | 'failure'>
    return { id, project_id: 'prj_1', ...body, created_at: at, updated_at: at }
  }
  vi.mocked(createExperimentDraft).mockReset().mockImplementation(kept('draft-1'))
  vi.mocked(updateExperimentDraft).mockReset().mockImplementation(kept('draft-1'))
  vi.mocked(deleteExperimentDraft).mockReset().mockResolvedValue()
  vi.mocked(fetchAgentTypes).mockResolvedValue(CATALOG)
})

describe('a new platform experiment', () => {
  it('is run by the platform with its defaults in one create: a sandboxed coordinator, max_agents generalists to start with, and the platform\'s ladder', async () => {
    show()
    await essentials()
    await create()
    await waitFor(() => expect(onSaved).toHaveBeenCalledExactlyOnceWith(created, true))
    expect(sent()).toMatchObject({
      name: 'Run', description: 'The brief', coordinator_in_agentsandbox: true, max_agents: 2, duration_hours: 48, budget_accelerator_hours: 10,
      researchers: [{ agent_type: 'generalist', count: 2 }],
    })
    for (const left of ['stages', 'report_interval_seconds', 'starts_at', 'repository']) expect(sent()).not.toHaveProperty(left)
    expect(createPlatformExperiment).toHaveBeenCalledTimes(1)
    expect(addPlatformExperimentAgents).not.toHaveBeenCalled()
  })

  it('sends what the person states over the defaults', async () => {
    show()
    await userEvent.type(name(), 'Run')
    await userEvent.type(screen.getByLabelText('Brief'), 'The brief')
    await next()
    await userEvent.clear(screen.getByLabelText('Duration (hours)'))
    await userEvent.type(screen.getByLabelText('Duration (hours)'), '6')
    await next()
    await userEvent.clear(screen.getByLabelText('Max researchers'))
    await userEvent.type(screen.getByLabelText('Max researchers'), '3')
    await create()
    await waitFor(() => expect(onSaved).toHaveBeenCalled())
    expect(sent()).toMatchObject({ duration_hours: 6, max_agents: 3, researchers: [{ agent_type: 'generalist', count: 3 }] })
    expect(addPlatformExperimentAgents).not.toHaveBeenCalled()
  })

  it('starts with the agents the person lists, within max researchers', async () => {
    show()
    await userEvent.type(name(), 'Run')
    await userEvent.type(screen.getByLabelText('Brief'), 'The brief')
    await next()
    await next()
    await userEvent.clear(screen.getByLabelText('Max researchers'))
    await userEvent.type(screen.getByLabelText('Max researchers'), '4')
    await userEvent.click(within(screen.getByRole('group', { name: 'Starting agents choice' })).getByRole('button', { name: 'Custom' }))
    await userEvent.clear(await screen.findByLabelText('Starting agent 1 count'))
    await userEvent.type(screen.getByLabelText('Starting agent 1 count'), '2')
    await userEvent.click(screen.getByRole('button', { name: '+ Add agents' }))
    await userEvent.type(screen.getByLabelText('Starting agent 2 model'), 'model-2')
    await create()
    await waitFor(() => expect(onSaved).toHaveBeenCalled())
    expect(sent()).toMatchObject({ max_agents: 4, researchers: [{ agent_type: 'generalist', count: 2 }, { agent_type: 'generalist', count: 1, model: 'model-2' }] })
  })

  it('offers the org\'s researcher types from the API, each with what it does, and links to manage them', async () => {
    show()
    await userEvent.type(name(), 'Run')
    await userEvent.type(screen.getByLabelText('Brief'), 'The brief')
    await next()
    await next()
    await userEvent.click(within(screen.getByRole('group', { name: 'Starting agents choice' })).getByRole('button', { name: 'Custom' }))
    const type = await screen.findByLabelText('Starting agent 1 type')
    expect(screen.getByText(/No fixed axis/)).toBeTruthy()
    await userEvent.click(type)
    const offered = (await screen.findAllByRole('option')).map(o => o.textContent)
    expect(offered).toEqual(['Generalist', 'Hyperparameter search', 'Architecture search', 'Data search', 'LR sweeper (v3)'])
    await userEvent.click(screen.getByRole('option', { name: 'LR sweeper (v3)' }))
    expect(screen.getByText("Your org's own researcher type, made by Ada.")).toBeTruthy()
    expect(screen.getByRole('link', { name: 'Manage agent types' }).getAttribute('href')).toBe('/orgs/org_a/settings/agent-types')
    await create()
    await waitFor(() => expect(onSaved).toHaveBeenCalled())
    expect(sent()).toMatchObject({ researchers: [{ agent_type: 'at-lr', count: 1 }] })
  })

  it('says starting agents beyond max researchers before the API does', async () => {
    show()
    await userEvent.type(name(), 'Run')
    await userEvent.type(screen.getByLabelText('Brief'), 'The brief')
    await next()
    await next()
    await userEvent.click(within(screen.getByRole('group', { name: 'Starting agents choice' })).getByRole('button', { name: 'Custom' }))
    await userEvent.clear(await screen.findByLabelText('Starting agent 1 count'))
    await userEvent.type(screen.getByLabelText('Starting agent 1 count'), '5')
    expect(await screen.findByText('5 starting agents, more than the 2 researchers allowed at once.')).toBeTruthy()
    expect(createPlatformExperiment).not.toHaveBeenCalled()
  })

  it('adds a common metric or a typed key from one control, a typed key maximizing', async () => {
    show()
    await userEvent.type(name(), 'Run')
    await userEvent.type(screen.getByLabelText('Brief'), 'The brief')
    await next()
    const metric = screen.getByLabelText('Add a metric')
    await userEvent.type(metric, 'val_loss')
    await userEvent.click(await screen.findByRole('option', { name: /Val Loss/ }))
    await userEvent.type(metric, 'widget_score{Enter}')
    await next()
    await create()
    await waitFor(() => expect(onSaved).toHaveBeenCalled())
    expect(sent().metrics).toEqual([{ key: 'val_loss', direction: 'minimize' }, { key: 'widget_score', direction: 'maximize' }])
  })

  it('states a gate with its bound, a tracked metric and the target of the ranked one, signed in its direction', async () => {
    show()
    await userEvent.type(name(), 'Run')
    await userEvent.type(screen.getByLabelText('Brief'), 'The brief')
    await next()
    const metric = screen.getByLabelText('Add a metric')
    await userEvent.type(metric, 'val_loss')
    await userEvent.click(await screen.findByRole('option', { name: /Val Loss/ }))
    await userEvent.type(metric, 'diverged{Enter}')
    await userEvent.type(metric, 'step{Enter}')
    await userEvent.type(screen.getByLabelText('Target of val_loss, percent past the baseline'), '2')
    await userEvent.click(screen.getByLabelText('Role of diverged'))
    await userEvent.click(await screen.findByRole('option', { name: 'Gate' }))
    await userEvent.click(screen.getByLabelText('Role of step'))
    await userEvent.click(await screen.findByRole('option', { name: 'Tracked' }))
    await next()
    await create()
    expect(await screen.findByText('Every gate needs a bound.')).toBeTruthy()
    await userEvent.click(screen.getByRole('tab', { name: '2. Measure' }))
    await userEvent.type(screen.getByLabelText('Bound of diverged'), '0')
    await next()
    await create()
    await waitFor(() => expect(onSaved).toHaveBeenCalled())
    expect(sent().metrics).toEqual([
      { key: 'val_loss', direction: 'minimize', role: 'ranking', target_vs_baseline: -0.02 },
      { key: 'diverged', direction: 'maximize', role: 'constraint', bound: 0 },
      { key: 'step', direction: 'maximize', role: 'attribute' },
    ])
  })

  it('shows a refused create in the API\'s words at the step it concerns, keeps the input as a draft naming why, and tries again', async () => {
    vi.mocked(createPlatformExperiment).mockRejectedValueOnce(new ApiError(422, 'the installation does not cover acme/widgets', 'repository_not_reachable'))
    show()
    await essentials({ repository: 'acme/widgets' })
    await create()
    expect(await screen.findByText('the installation does not cover acme/widgets')).toBeTruthy()
    expect(onSaved).not.toHaveBeenCalled()
    expect(addPlatformExperimentAgents).not.toHaveBeenCalled()
    await waitFor(() => expect(createExperimentDraft).toHaveBeenCalledTimes(1))
    expect(vi.mocked(createExperimentDraft).mock.calls[0]).toEqual(['prj_1', {
      form: expect.objectContaining({ name: 'Run', description: 'The brief', repository: 'acme/widgets' }), step: '1', failure: 'the installation does not cover acme/widgets',
    }])
    expect(screen.getByLabelText('Brief')).toBeTruthy()
    expect(screen.getByText('Not created. Change what the refusal names and try again.')).toBeTruthy()

    await userEvent.click(screen.getByRole('button', { name: 'Try again' }))
    await waitFor(() => expect(onSaved).toHaveBeenCalledExactlyOnceWith(created, true))
    expect(deleteExperimentDraft).toHaveBeenCalledExactlyOnceWith('prj_1', 'draft-1')
  })

})

describe('a repository that brings its own experiment', () => {
  // A person who picked such a repository in the form started agents with an empty brief: the
  // form knew nothing of the spec the repository keeps.
  it('fills the form from its experiment file, and what is created carries it', async () => {
    vi.mocked(fetchExperimentFile).mockResolvedValue({
      repository: 'acme/widgets', name: 'Widget compression', description: 'Make widgets smaller.', report_interval_seconds: 10,
      budget_accelerator_hours: 3, max_agents: 4, duration_hours: 6,
      metrics: [{ key: 'compression_ratio', direction: 'maximize', role: 'ranking' }], stages: [{ length_pct: 100, evict_pct: 0, max_job_hours: 0 }],
    })
    show()
    await chooseRepository('acme/widgets')
    await waitFor(() => expect(name()).toHaveProperty('value', 'Widget compression'))
    expect(fetchExperimentFile).toHaveBeenCalledExactlyOnceWith('prj_1', 'acme/widgets', 'main')
    await next()
    await next()
    await create()
    await waitFor(() => expect(onSaved).toHaveBeenCalled())
    expect(sent()).toMatchObject({
      name: 'Widget compression', description: 'Make widgets smaller.', report_interval_seconds: 10, repository: 'acme/widgets', branch: 'main',
      budget_accelerator_hours: 3, max_agents: 4, duration_hours: 6,
      metrics: [{ key: 'compression_ratio', direction: 'maximize', role: 'ranking' }], stages: [{ length_pct: 100, evict_pct: 0, max_job_hours: 0 }],
    })
  })

  it('keeps a name already typed when the repository has no experiment file', async () => {
    show()
    await userEvent.type(name(), 'Mine')
    await chooseRepository('acme/widgets')
    await waitFor(() => expect(fetchExperimentFile).toHaveBeenCalled())
    expect(name()).toHaveProperty('value', 'Mine')
  })
})

describe('editing a platform experiment', () => {
  const existing = { id: 'pe-1', project_id: 'prj_1', name: 'Old', description: 'the brief', status: 'open', budget_accelerator_hours: 10, max_agents: 2, starts_at: at, duration_hours: 12, stages: [], shared: false,
    editable: ['name', 'description', 'budget_accelerator_hours', 'max_agents', 'researchers', 'max_concurrent_accelerators', 'metrics', 'report_interval_seconds', 'starts_at', 'duration_hours', 'baseline_job_id', 'baseline_reason'] } as unknown as PlatformExperiment

  it('sends the fields that changed and no others, from any step', async () => {
    const renamed = { ...existing, name: 'New' }
    vi.mocked(updatePlatformExperiment).mockResolvedValue(renamed)
    show(existing)
    await userEvent.clear(name())
    await userEvent.type(name(), 'New')
    await userEvent.click(screen.getByRole('button', { name: 'Save changes' }))
    await waitFor(() => expect(onSaved).toHaveBeenCalledExactlyOnceWith(renamed))
    expect(updatePlatformExperiment).toHaveBeenCalledExactlyOnceWith('pe-1', { name: 'New' })
    expect(createPlatformExperiment).not.toHaveBeenCalled()
  })

  it('shows max researchers without changing it: that is done in place on the experiment page', async () => {
    show(existing)
    await userEvent.click(screen.getByRole('tab', { name: '3. Budget and rules' }))
    expect(screen.queryByRole('spinbutton', { name: 'Max researchers' })).toBeNull()
    expect(screen.getByText('Changed on the experiment page, under Researchers, with the pool.')).toBeTruthy()
  })

  it('reads the experiment\'s duration and sends a changed one as duration_hours', async () => {
    vi.mocked(updatePlatformExperiment).mockResolvedValue(existing)
    show(existing)
    await next()
    const duration = screen.getByLabelText('Duration (hours)')
    expect(duration).toHaveProperty('value', '12')
    await userEvent.clear(duration)
    await userEvent.type(duration, '24')
    await userEvent.click(screen.getByRole('button', { name: 'Save changes' }))
    await waitFor(() => expect(updatePlatformExperiment).toHaveBeenCalledExactlyOnceWith('pe-1', { duration_hours: 24 }))
  })

  it('refuses to empty a setting the update cannot unset, and sends a changed one', async () => {
    const set = { ...existing, report_interval_seconds: 30, max_concurrent_accelerators: 4 } as PlatformExperiment
    vi.mocked(updatePlatformExperiment).mockResolvedValue(set)
    show(set)
    await userEvent.click(screen.getByRole('tab', { name: '3. Budget and rules' }))
    await userEvent.click(screen.getByRole('button', { name: 'More settings' }))
    await userEvent.clear(screen.getByLabelText('Report interval (seconds)'))
    await userEvent.clear(screen.getByLabelText('Max concurrent accelerators'))
    await userEvent.click(screen.getByRole('button', { name: 'Save changes' }))
    expect(await screen.findByRole('alert')).toBeTruthy()
    expect(updatePlatformExperiment).not.toHaveBeenCalled()
    await userEvent.type(screen.getByLabelText('Report interval (seconds)'), '60')
    await userEvent.type(screen.getByLabelText('Max concurrent accelerators'), '4')
    await userEvent.click(screen.getByRole('button', { name: 'Save changes' }))
    await waitFor(() => expect(updatePlatformExperiment).toHaveBeenCalledExactlyOnceWith('pe-1', { report_interval_seconds: 60 }))
  })
})

describe('a draft of a new platform experiment', () => {
  const draft: ExperimentDraft = {
    id: 'draft-7', project_id: 'prj_1', step: '2', failure: '', created_at: at, updated_at: at,
    form: { name: 'Half done', description: 'A brief', repository: '', budget_accelerator_hours: '3' },
  }

  it('is saved from any step, as typed, and saved again in place', async () => {
    show()
    await userEvent.type(name(), 'Run')
    await next()
    await userEvent.click(screen.getByRole('button', { name: 'Save draft' }))
    expect(await screen.findByText('Draft saved.')).toBeTruthy()
    expect(vi.mocked(createExperimentDraft).mock.calls[0]).toEqual(['prj_1', { form: expect.objectContaining({ name: 'Run', budget_accelerator_hours: '10' }), step: '2', failure: '' }])
    await next()
    await userEvent.click(screen.getByRole('button', { name: 'Save draft' }))
    await waitFor(() => expect(updateExperimentDraft).toHaveBeenCalledWith('prj_1', 'draft-1', expect.objectContaining({ step: '3' })))
    expect(createExperimentDraft).toHaveBeenCalledTimes(1)
  })

  it('opens at its step with what was typed, and goes once the experiment is created from it', async () => {
    show(undefined, draft)
    expect(screen.getByRole('tab', { name: '2. Measure', selected: true })).toBeTruthy()
    await next()
    await create()
    await waitFor(() => expect(onSaved).toHaveBeenCalledExactlyOnceWith(created, true))
    expect(sent()).toMatchObject({ name: 'Half done', description: 'A brief', budget_accelerator_hours: 3 })
    expect(deleteExperimentDraft).toHaveBeenCalledExactlyOnceWith('prj_1', 'draft-7')
  })

  it('stays while the create is refused, and shows why it was refused last', async () => {
    vi.mocked(createPlatformExperiment).mockRejectedValue(new ApiError(422, 'budget over the org\'s limit', 'quota_exceeded'))
    show(undefined, { ...draft, failure: 'repository creation failed' })
    expect(screen.getByText('repository creation failed')).toBeTruthy()
    await userEvent.click(screen.getByRole('button', { name: 'Try again' }))
    expect(await screen.findByText('budget over the org\'s limit')).toBeTruthy()
    await waitFor(() => expect(updateExperimentDraft).toHaveBeenCalledWith('prj_1', 'draft-7', expect.objectContaining({ step: '3', failure: 'budget over the org\'s limit' })))
    expect(deleteExperimentDraft).not.toHaveBeenCalled()
  })

  it('is left as it is while the App is installed in another tab', async () => {
    const opened = { location: { href: '' }, close: vi.fn() }
    vi.spyOn(window, 'open').mockReturnValue(opened as unknown as Window)
    vi.mocked(beginGitHubInstall).mockResolvedValue({ state: 's-1', install_url: 'https://github.test/install?state=s-1', expires_at: '' })
    show()
    await userEvent.type(name(), 'Before GitHub')
    await userEvent.click(await screen.findByRole('button', { name: 'give the App access' }))
    expect(await screen.findByText(/Waiting for GitHub/)).toBeTruthy()
    expect(opened.location.href).toBe('https://github.test/install?state=s-1')
    expect(createExperimentDraft).not.toHaveBeenCalled()
    expect(name()).toHaveProperty('value', 'Before GitHub')
  })
})
