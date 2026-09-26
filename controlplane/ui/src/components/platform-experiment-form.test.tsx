import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import {
  ApiError, addPlatformExperimentAgents, createPlatformExperiment, fetchExperimentFile, fetchGitHubInstallations, fetchGitHubRepositories, startPlatformExperiment,
  updatePlatformExperiment,
} from '@/lib/api'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import { PlatformExperimentForm } from '@/components/platform-experiment-form'
import type { Me, PlatformExperiment } from '@/types'

vi.mock('next/navigation', () => ({ useSearchParams: () => new URLSearchParams('project=prj_1') }))
vi.mock('@/lib/api', async importActual => ({
  ...(await importActual<typeof import('@/lib/api')>()),
  addPlatformExperimentAgents: vi.fn(), createPlatformExperiment: vi.fn(), fetchExperimentFile: vi.fn(), fetchGitHubInstallations: vi.fn(), fetchGitHubRepositories: vi.fn(),
  startPlatformExperiment: vi.fn(), updatePlatformExperiment: vi.fn(),
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

async function essentials({ repository }: { repository?: string } = {}) {
  await userEvent.type(screen.getByPlaceholderText('e.g. Q3 Vision Benchmark'), 'Run')
  if (repository) {
    await userEvent.click(await screen.findByLabelText('Repository'))
    await userEvent.click(await screen.findByRole('option', { name: repository }))
  }
  await userEvent.click(screen.getByRole('button', { name: 'Next: competition settings' }))
}

beforeEach(() => {
  onSaved.mockReset()
  vi.mocked(fetchExperimentFile).mockReset().mockRejectedValue(new ApiError(422, 'the repository has no experiment file', 'no_experiment_file'))
  vi.mocked(fetchGitHubInstallations).mockResolvedValue({ items: [{ org_id: 'org_a', installation_id: 7, account_login: 'acme', connected_by: 'u9', connected_by_name: 'Una', connected_at: at }], total: 1 })
  vi.mocked(fetchGitHubRepositories).mockResolvedValue({ items: [{ repository: 'acme/widgets', installation_id: 7, private: false }], total: 1 })
  vi.mocked(createPlatformExperiment).mockResolvedValue(created)
  vi.mocked(addPlatformExperimentAgents).mockResolvedValue([])
  vi.mocked(startPlatformExperiment).mockResolvedValue()
})

describe('a new platform experiment whose agents the operator runs', () => {
  it('is created and nothing else, which is the choice the form opens with', async () => {
    show()
    await essentials()
    expect(screen.getByRole('button', { name: 'I run my own agents' }).getAttribute('aria-pressed')).toBe('true')
    expect(screen.queryByLabelText('Researchers')).toBeNull()
    await userEvent.click(screen.getByRole('button', { name: 'Create Experiment' }))
    await waitFor(() => expect(onSaved).toHaveBeenCalledExactlyOnceWith(created))
    const sent = vi.mocked(createPlatformExperiment).mock.calls[0][1]
    expect(sent.coordinator_hosted).toBeUndefined()
    // It starts when it is started, and the report interval is the platform's.
    expect(sent.starts_at).toBeUndefined()
    expect(sent.report_interval_seconds).toBeUndefined()
    expect(new Date(sent.ends_at!).getTime()).toBeGreaterThan(Date.now())
    expect(addPlatformExperimentAgents).not.toHaveBeenCalled()
    expect(startPlatformExperiment).not.toHaveBeenCalled()
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
    await userEvent.click(await screen.findByLabelText('Repository'))
    await userEvent.click(await screen.findByRole('option', { name: 'acme/widgets' }))
    await screen.findByText(/Filled in from \.hypothesisloop\/experiment\.yaml of acme\/widgets/)
    expect(fetchExperimentFile).toHaveBeenCalledExactlyOnceWith('prj_1', 'acme/widgets')
    expect(screen.getByPlaceholderText('e.g. Q3 Vision Benchmark')).toHaveProperty('value', 'Widget compression')
    await userEvent.click(screen.getByRole('button', { name: 'Next: competition settings' }))
    await userEvent.click(screen.getByRole('button', { name: 'Hosted agents' }))
    await userEvent.click(screen.getByRole('button', { name: 'Create and run' }))
    await waitFor(() => expect(onSaved).toHaveBeenCalled())
    expect(vi.mocked(createPlatformExperiment).mock.calls[0][1]).toMatchObject({
      name: 'Widget compression', description: 'Make widgets smaller.', report_interval_seconds: 10, repository: 'acme/widgets',
      metrics: [{ key: 'compression_ratio', direction: 'maximize', role: 'ranking' }], stages: [{ length_pct: 100, evict_pct: 0, max_job_hours: 0 }],
    })
  })

  it('keeps a name already typed, and says so when the repository has no experiment file', async () => {
    show()
    await userEvent.type(screen.getByPlaceholderText('e.g. Q3 Vision Benchmark'), 'Mine')
    await userEvent.click(await screen.findByLabelText('Repository'))
    await userEvent.click(await screen.findByRole('option', { name: 'acme/widgets' }))
    await screen.findByText(/acme\/widgets has no \.hypothesisloop\/experiment\.yaml/)
    expect(screen.getByPlaceholderText('e.g. Q3 Vision Benchmark')).toHaveProperty('value', 'Mine')
  })
})

describe('a new platform experiment with hosted agents', () => {
  it('is created with its repository and a hosted coordinator, given its researchers and started', async () => {
    show()
    await essentials({ repository: 'acme/widgets' })
    await userEvent.click(screen.getByRole('button', { name: 'Hosted agents' }))
    const researchers = screen.getByLabelText('Researchers')
    expect(researchers).toHaveProperty('value', '2')
    await userEvent.clear(researchers)
    await userEvent.type(researchers, '3')
    await userEvent.click(screen.getByRole('button', { name: 'Create and run' }))
    await waitFor(() => expect(onSaved).toHaveBeenCalledExactlyOnceWith(created, { failure: undefined }))
    expect(vi.mocked(createPlatformExperiment).mock.calls[0][1]).toMatchObject({
      name: 'Run', repository: 'acme/widgets', coordinator_hosted: true,
    })
    expect(vi.mocked(createPlatformExperiment).mock.calls[0][1].starts_at).toBeUndefined()
    // Nobody else joins a run the platform staffs, so the cap is the researchers asked for.
    expect(vi.mocked(createPlatformExperiment).mock.calls[0][1].max_agents).toBe(3)
    expect(new Date(vi.mocked(createPlatformExperiment).mock.calls[0][1].ends_at!).getTime()).toBeLessThan(Date.now() + 3 * 86_400_000)
    expect(addPlatformExperimentAgents).toHaveBeenCalledTimes(3)
    expect(startPlatformExperiment).toHaveBeenCalledExactlyOnceWith('pe-new')
  })

  // Hosting needed a repository, so a person with an idea and no repository could not start.
  it('is created with no repository when none is chosen', async () => {
    show()
    await essentials()
    await userEvent.click(screen.getByRole('button', { name: 'Hosted agents' }))
    await userEvent.click(screen.getByRole('button', { name: 'Create and run' }))
    await waitFor(() => expect(onSaved).toHaveBeenCalled())
    const sent = vi.mocked(createPlatformExperiment).mock.calls[0][1]
    expect(sent).toMatchObject({ name: 'Run', coordinator_hosted: true })
    expect(sent.repository).toBeUndefined()
    expect(addPlatformExperimentAgents).toHaveBeenCalledTimes(2)
  })

  it('shows a refused create in the API\'s words and stays on the form, with nothing else called', async () => {
    vi.mocked(createPlatformExperiment).mockRejectedValue(new Error('the installation does not cover acme/widgets'))
    show()
    await essentials({ repository: 'acme/widgets' })
    await userEvent.click(screen.getByRole('button', { name: 'Hosted agents' }))
    await userEvent.click(screen.getByRole('button', { name: 'Create and run' }))
    expect(await screen.findByText('the installation does not cover acme/widgets')).toBeTruthy()
    expect(onSaved).not.toHaveBeenCalled()
    expect(addPlatformExperimentAgents).not.toHaveBeenCalled()
  })

  it('hands on the experiment with the step the API stopped at, since the experiment exists', async () => {
    vi.mocked(addPlatformExperimentAgents).mockRejectedValue(new Error('the platform runs at most 1 machine'))
    show()
    await essentials({ repository: 'acme/widgets' })
    await userEvent.click(screen.getByRole('button', { name: 'Hosted agents' }))
    await userEvent.click(screen.getByRole('button', { name: 'Create and run' }))
    await waitFor(() => expect(onSaved).toHaveBeenCalledExactlyOnceWith(created, {
      failure: { step: 'adding hosted agent 1 of 2', message: 'the platform runs at most 1 machine' },
    }))
    expect(startPlatformExperiment).not.toHaveBeenCalled()
  })
})

describe('editing a platform experiment', () => {
  it('offers no choice of who runs it, and sends the fields that changed and no others', async () => {
    const existing = { id: 'pe-1', project_id: 'prj_1', name: 'Old', description: 'the brief', status: 'open', budget_accelerator_hours: 10, max_agents: 2, starts_at: at, ends_at: '2027-01-01T00:00:00Z', stages: [], shared: false } as unknown as PlatformExperiment
    const renamed = { ...existing, name: 'New' }
    vi.mocked(updatePlatformExperiment).mockReset().mockResolvedValue(renamed)
    show(existing)
    const name = screen.getByPlaceholderText('e.g. Q3 Vision Benchmark')
    await userEvent.clear(name)
    await userEvent.type(name, 'New')
    await userEvent.click(screen.getByRole('tab', { name: '2. Competition settings' }))
    expect(screen.queryByRole('group', { name: 'Who runs the agents?' })).toBeNull()
    // The settings few people change are out of the way until asked for.
    expect(screen.queryByLabelText('Report interval (seconds)')).toBeNull()
    await userEvent.click(screen.getByRole('button', { name: 'More settings' }))
    expect(screen.getByLabelText('Report interval (seconds)')).toBeTruthy()
    expect(screen.getByLabelText('Starts at')).toBeTruthy()
    expect(screen.getByLabelText('Max concurrent accelerators')).toBeTruthy()
    await userEvent.click(screen.getByRole('button', { name: 'Save Changes' }))
    await waitFor(() => expect(onSaved).toHaveBeenCalledExactlyOnceWith(renamed))
    expect(updatePlatformExperiment).toHaveBeenCalledExactlyOnceWith('pe-1', { name: 'New' })
    expect(createPlatformExperiment).not.toHaveBeenCalled()
  })
})
