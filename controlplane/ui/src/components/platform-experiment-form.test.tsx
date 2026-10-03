import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import {
  ApiError, addPlatformExperimentAgents, createPlatformExperiment, fetchExperimentFile, fetchGitHubApp, fetchGitHubInstallations, fetchGitHubRepositories,
  updatePlatformExperiment,
} from '@/lib/api'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import { PlatformExperimentForm } from '@/components/platform-experiment-form'
import type { Me, PlatformExperiment } from '@/types'

vi.mock('next/navigation', () => ({ useSearchParams: () => new URLSearchParams('project=prj_1') }))
vi.mock('@/lib/api', async importActual => ({
  ...(await importActual<typeof import('@/lib/api')>()),
  addPlatformExperimentAgents: vi.fn(), createPlatformExperiment: vi.fn(), fetchExperimentFile: vi.fn(), fetchGitHubApp: vi.fn(), fetchGitHubInstallations: vi.fn(), fetchGitHubRepositories: vi.fn(),
  updatePlatformExperiment: vi.fn(),
}))

const at = '2026-01-01T00:00:00Z'
const me: Me = {
  kind: 'user', user_id: 'u1', name: 'Ada',
  orgs: [{ id: 'org_a', name: 'Lab', role: 'member', created_at: at }],
  projects: [{ id: 'prj_1', org_id: 'org_a', name: 'default', created_at: at }],
}
const created = { id: 'pe-new', project_id: 'prj_1', name: 'Run' } as PlatformExperiment
const onSaved = vi.fn()

function show(initial?: PlatformExperiment) {
  const resolved = resolveTenant(me, '')
  if (resolved.state !== 'selected') throw new Error('no tenant')
  return render(
    <SWRConfig value={{ provider: () => new Map() }}>
      <TenantContext.Provider value={resolved.tenant}><PlatformExperimentForm initial={initial} onSaved={onSaved} /></TenantContext.Provider>
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
  await userEvent.type(screen.getByPlaceholderText(/^Goals, evaluation protocol/), 'The brief')
  if (repository) await chooseRepository(repository)
  await next()
  await next()
}

beforeEach(() => {
  onSaved.mockReset()
  vi.mocked(fetchExperimentFile).mockReset().mockRejectedValue(new ApiError(422, 'the repository has no experiment file', 'no_experiment_file'))
  vi.mocked(fetchGitHubApp).mockResolvedValue({ slug: 'hl', client_id: 'c', install_url: 'https://github.test/install', authorize_url: 'https://github.test/authorize', new_repository_url: 'https://github.test/new' })
  vi.mocked(fetchGitHubInstallations).mockResolvedValue({ items: [{ org_id: 'org_a', installation_id: 7, account_login: 'acme', connected_actor: { kind: 'user', id: 'u9', name: 'Una' }, connected_at: at }], total: 1 })
  vi.mocked(fetchGitHubRepositories).mockResolvedValue({ items: [{ repository: 'acme/widgets', installation_id: 7, private: false }], total: 1 })
  vi.mocked(createPlatformExperiment).mockReset().mockResolvedValue(created)
  vi.mocked(addPlatformExperimentAgents).mockReset().mockResolvedValue([])
  vi.mocked(updatePlatformExperiment).mockReset()
})

describe('a new platform experiment', () => {
  it('is run by the platform with its defaults: a hosted coordinator, the researchers as max_agents, and the platform\'s ladder', async () => {
    show()
    await essentials()
    await create()
    await waitFor(() => expect(onSaved).toHaveBeenCalledExactlyOnceWith(created, { failure: undefined }))
    expect(sent()).toMatchObject({ name: 'Run', description: 'The brief', coordinator_hosted: true, max_agents: 2, duration_hours: 48, budget_accelerator_hours: 10 })
    for (const left of ['stages', 'report_interval_seconds', 'starts_at', 'repository']) expect(sent()).not.toHaveProperty(left)
    expect(addPlatformExperimentAgents).toHaveBeenCalledTimes(2)
  })

  it('sends what the person states over the defaults', async () => {
    show()
    await userEvent.type(name(), 'Run')
    await userEvent.type(screen.getByPlaceholderText(/^Goals, evaluation protocol/), 'The brief')
    await next()
    await userEvent.clear(screen.getByLabelText('Duration (hours)'))
    await userEvent.type(screen.getByLabelText('Duration (hours)'), '6')
    await next()
    await userEvent.clear(screen.getByLabelText('Researchers'))
    await userEvent.type(screen.getByLabelText('Researchers'), '3')
    await create()
    await waitFor(() => expect(onSaved).toHaveBeenCalled())
    expect(sent()).toMatchObject({ duration_hours: 6, max_agents: 3 })
    expect(addPlatformExperimentAgents).toHaveBeenCalledTimes(3)
  })

  it('adds a common metric or a typed key from one control, a typed key maximizing', async () => {
    show()
    await userEvent.type(name(), 'Run')
    await userEvent.type(screen.getByPlaceholderText(/^Goals, evaluation protocol/), 'The brief')
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

  // GitHub may refuse the platform a new repository. The form returns to where one is chosen
  // and says so beside the way on: create it on GitHub, refresh, choose it.
  it('returns to the repository when GitHub would not let the platform create one, and creates with the one then chosen', async () => {
    vi.mocked(createPlatformExperiment).mockRejectedValueOnce(new ApiError(422, 'the owner connects the account again, or creates a private repository on it', 'repository_authorization_required'))
    show()
    await essentials()
    await create()
    expect((await screen.findByRole('alert')).textContent).toBe('The platform could not create the repository: the owner connects the account again, or creates a private repository on it')
    expect(screen.getByRole('link', { name: 'create it on GitHub' }).getAttribute('href')).toBe('https://github.test/new?owner=acme&visibility=private')
    expect(name()).toHaveProperty('value', 'Run')
    expect(onSaved).not.toHaveBeenCalled()

    await userEvent.click(screen.getByRole('button', { name: 'refresh this list' }))
    await chooseRepository('acme/widgets')
    expect(screen.queryByRole('alert')).toBeNull()
    await next()
    await next()
    await create()
    await waitFor(() => expect(onSaved).toHaveBeenCalledExactlyOnceWith(created, { failure: undefined }))
    expect(sent(1).repository).toBe('acme/widgets')
  })

  it('shows a refused create in the API\'s words and stays on the form, with nothing else called', async () => {
    vi.mocked(createPlatformExperiment).mockRejectedValue(new Error('the installation does not cover acme/widgets'))
    show()
    await essentials({ repository: 'acme/widgets' })
    await create()
    expect(await screen.findByText('the installation does not cover acme/widgets')).toBeTruthy()
    expect(onSaved).not.toHaveBeenCalled()
    expect(addPlatformExperimentAgents).not.toHaveBeenCalled()
  })

  it('hands on the experiment with the step the API stopped at, since the experiment exists', async () => {
    vi.mocked(addPlatformExperimentAgents).mockRejectedValue(new Error('the platform runs at most 1 machine'))
    show()
    await essentials({ repository: 'acme/widgets' })
    await create()
    await waitFor(() => expect(onSaved).toHaveBeenCalledExactlyOnceWith(created, {
      failure: { step: 'adding hosted agent 1 of 2', message: 'the platform runs at most 1 machine' },
    }))
  })
})

describe('a repository that brings its own experiment', () => {
  // A person who picked such a repository in the form started agents with an empty brief: the
  // form knew nothing of the file the API reads for a create with from_repo.
  it('fills the form from its experiment file, and what is created carries it', async () => {
    vi.mocked(fetchExperimentFile).mockResolvedValue({
      repository: 'acme/widgets', name: 'Widget compression', description: 'Make widgets smaller.', report_interval_seconds: 10,
      metrics: [{ key: 'compression_ratio', direction: 'maximize', role: 'ranking' }], stages: [{ length_pct: 100, evict_pct: 0, max_job_hours: 0 }],
    })
    show()
    await chooseRepository('acme/widgets')
    await waitFor(() => expect(name()).toHaveProperty('value', 'Widget compression'))
    expect(fetchExperimentFile).toHaveBeenCalledExactlyOnceWith('prj_1', 'acme/widgets')
    await next()
    await next()
    await create()
    await waitFor(() => expect(onSaved).toHaveBeenCalled())
    expect(sent()).toMatchObject({
      name: 'Widget compression', description: 'Make widgets smaller.', report_interval_seconds: 10, repository: 'acme/widgets',
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
  const existing = { id: 'pe-1', project_id: 'prj_1', name: 'Old', description: 'the brief', status: 'open', budget_accelerator_hours: 10, max_agents: 2, starts_at: at, duration_hours: 12, stages: [], shared: false } as unknown as PlatformExperiment

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
