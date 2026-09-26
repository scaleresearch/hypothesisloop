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
  vi.mocked(fetchGitHubInstallations).mockResolvedValue({ items: [{ org_id: 'org_a', installation_id: 7, account_login: 'acme', connected_by: 'u9', connected_at: at }], total: 1 })
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
    expect(new Date(sent.starts_at).getTime()).toBeGreaterThan(Date.now())
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
      name: 'Run', repository: 'acme/widgets', coordinator_hosted: true, starts_at: '0001-01-01T00:00:00Z',
    })
    expect(addPlatformExperimentAgents).toHaveBeenCalledTimes(3)
    expect(startPlatformExperiment).toHaveBeenCalledExactlyOnceWith('pe-new')
  })

  it('cannot be asked for without a repository, and says what is missing', async () => {
    show()
    await essentials()
    await userEvent.click(screen.getByRole('button', { name: 'Hosted agents' }))
    expect(screen.getByText(/Hosted agents and the coordinator work in the experiment's repository\. Choose one under Essentials first\./)).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Create and run' })).toHaveProperty('disabled', true)
    expect(createPlatformExperiment).not.toHaveBeenCalled()
  })

  it('does not use the start time, and says so', async () => {
    show()
    await essentials({ repository: 'acme/widgets' })
    await userEvent.click(screen.getByRole('button', { name: 'Hosted agents' }))
    await userEvent.click(screen.getByRole('tab', { name: '1. Essentials' }))
    expect(screen.getByLabelText('Starts at')).toHaveProperty('disabled', true)
    expect(screen.getByText('Not used: with hosted agents it starts as soon as they have joined.')).toBeTruthy()
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
  it('offers no choice of who runs it, and saves through the update', async () => {
    const existing = { id: 'pe-1', project_id: 'prj_1', name: 'Old', status: 'open', budget_accelerator_hours: 10, max_agents: 2, starts_at: at, ends_at: '2027-01-01T00:00:00Z', stages: [], shared: false } as unknown as PlatformExperiment
    vi.mocked(updatePlatformExperiment).mockResolvedValue(existing)
    show(existing)
    await userEvent.click(screen.getByRole('tab', { name: '2. Competition settings' }))
    expect(screen.queryByRole('group', { name: 'Who runs the agents?' })).toBeNull()
    await userEvent.click(screen.getByRole('button', { name: 'Save Changes' }))
    await waitFor(() => expect(onSaved).toHaveBeenCalledExactlyOnceWith(existing))
    expect(createPlatformExperiment).not.toHaveBeenCalled()
  })
})
