import { act, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SWRConfig } from 'swr'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { fetchAgentTypes } from '@/lib/api/agent-types'
import { fetchModels } from '@/lib/api/models'
import { addPlatformExperimentAgents } from '@/lib/api/agents'
import { createExperimentDraft, deleteExperimentDraft, updateExperimentDraft } from '@/lib/api/drafts'
import { beginGitHubInstall, fetchGitHubApp, fetchGitHubInstallations, fetchGitHubRepositories } from '@/lib/api/github'
import { createPlatformExperiment, fetchExperimentFile, updatePlatformExperiment } from '@/lib/api/platform-experiments'
import { ApiError } from '@/lib/api/request'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import { PlatformExperimentForm } from '@/components/platform-experiment-form'
import { CATALOG } from '@/test-agent-types'
import { MODELS, pickModel } from '@/test-models'
import { choose } from '@/test-select'
import type { ExperimentDraft, Me, PlatformExperiment } from '@/types/api'

vi.mock('@/lib/api/agent-types', async importActual => ({
  ...(await importActual<typeof import('@/lib/api/agent-types')>()),
  fetchAgentTypes: vi.fn(),
  fetchAgentRoles: vi.fn(async () => (await import('@/test-agent-types')).ROLES),
}))
vi.mock('@/lib/api/models', () => ({ fetchModels: vi.fn() }))
vi.mock('@/lib/api/clusters', () => ({
  fetchResourceCapacity: vi.fn(async () => ({
    budget_reference: 'H100',
    clusters: [{ cluster_name: 'a', hosts: 1, multi_node: false, accelerators: [
      { accelerator_type: 'nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3', kind: 'accelerator', acch_per_hour: 1, available: 1, total: 1, per_node_available: 1 },
      { accelerator_type: 'nvidia.com/gpu.product=NVIDIA-GeForce-RTX-4090', kind: 'accelerator', acch_per_hour: 0.25, available: 1, total: 1, per_node_available: 1 },
    ] }],
  })),
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

let rendered: ReturnType<typeof render> | null = null

function show(initial?: PlatformExperiment, draft?: ExperimentDraft) {
  const resolved = resolveTenant(me, '')
  if (resolved.state !== 'selected') throw new Error('no tenant')
  rendered = render(
    <SWRConfig value={{ provider: () => new Map() }}>
      <TenantContext.Provider value={resolved.tenant}><PlatformExperimentForm initial={initial} draft={draft} onSaved={onSaved} /></TenantContext.Provider>
    </SWRConfig>,
  )
  return rendered
}

// A form taken off the page keeps what was typed as a draft and reads the drafts again through
// SWR. The page's SWR cache outlives the form, so the form leaves first and its save settles.
afterEach(async () => {
  rendered?.rerender(<SWRConfig value={{ provider: () => new Map() }}>{null}</SWRConfig>)
  rendered = null
  await act(() => new Promise(resolve => setTimeout(resolve, 0)))
})

const name = () => screen.getByPlaceholderText('e.g. Q3 Vision Benchmark')
const next = () => userEvent.click(screen.getByRole('button', { name: /^Next: / }))
const create = () => userEvent.click(screen.getByRole('button', { name: 'Create experiment' }))
const sent = (call = 0) => vi.mocked(createPlatformExperiment).mock.calls[call][1]

async function chooseRepository(repository: string) {
  await userEvent.click(await screen.findByLabelText('Repository'))
  await userEvent.click(await screen.findByRole('option', { name: repository }))
}

// The name and the brief, then on through the steps to the last.
async function essentials({ repository }: { repository?: string } = {}) {
  await userEvent.type(name(), 'Run')
  await userEvent.type(screen.getByLabelText('Experiment goal'), 'The brief')
  if (repository) await chooseRepository(repository)
  await next()
  await next()
}

beforeEach(() => {
  onSaved.mockReset()
  vi.mocked(fetchModels).mockResolvedValue(MODELS)
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
  it('is run by the platform with its defaults in one create: a sandboxed coordinator, a team of generalists and a researcher, and the platform\'s ladder', async () => {
    show()
    await essentials()
    await create()
    await waitFor(() => expect(onSaved).toHaveBeenCalledExactlyOnceWith(created, true))
    expect(sent()).toMatchObject({
      name: 'Run', description: 'The brief', coordinator_in_agentsandbox: true, duration_hours: 48, budget_accelerator_hours: 10,
      agents: [{ agent_type: 'generalist', count: 2 }, { agent_type: 'researcher', count: 1 }],
    })
    for (const left of ['stages', 'report_interval_seconds', 'starts_at', 'repository', 'coordinator_model', 'max_agents']) expect(sent()).not.toHaveProperty(left)
    expect(createPlatformExperiment).toHaveBeenCalledTimes(1)
    expect(addPlatformExperimentAgents).not.toHaveBeenCalled()
  })

  it('sends what the person states over the defaults', async () => {
    show()
    await userEvent.type(name(), 'Run')
    await userEvent.type(screen.getByLabelText('Experiment goal'), 'The brief')
    await next()
    await userEvent.clear(screen.getByLabelText('Duration (hours)'))
    await userEvent.type(screen.getByLabelText('Duration (hours)'), '6')
    await next()
    expect(screen.queryByLabelText('Max executors')).toBeNull()
    await userEvent.clear(await screen.findByLabelText('Team entry 1 count'))
    await userEvent.type(screen.getByLabelText('Team entry 1 count'), '3')
    await create()
    await waitFor(() => expect(onSaved).toHaveBeenCalled())
    // The team's executors are the most at work at once, which the API counts.
    expect(sent()).toMatchObject({ duration_hours: 6, agents: [{ agent_type: 'generalist', count: 3 }, { agent_type: 'researcher', count: 1 }] })
    expect(sent()).not.toHaveProperty('max_agents')
    expect(addPlatformExperimentAgents).not.toHaveBeenCalled()
  })

  it('composes the team in one list after its coordinator: entries of several executor types, each its count and model, the researcher on the coordinator\'s model', async () => {
    show()
    await userEvent.type(name(), 'Run')
    await userEvent.type(screen.getByLabelText('Experiment goal'), 'The brief')
    await next()
    await next()
    expect(await screen.findByText('2 executors, 1 researcher')).toBeTruthy()
    // A researcher's model defaults to the coordinator's when neither its entry nor its type names one.
    const researcherModel = screen.getByLabelText('Team entry 2 model') as HTMLInputElement
    expect(researcherModel.placeholder).toBe("Coordinator's model: anthropic/claude-opus-5-5")
    expect(screen.getByLabelText('Coordinator count')).toHaveProperty('disabled', true)
    await pickModel(screen.getByLabelText('Coordinator model'), 'sonnet', 'anthropic/claude-sonnet-5')
    expect(researcherModel.placeholder).toBe("Coordinator's model: anthropic/claude-sonnet-5")
    await userEvent.click(screen.getByRole('button', { name: '+ Add executor' }))
    await choose(screen.getByLabelText('Team entry 3 type'), 'Data search')
    await pickModel(screen.getByLabelText('Team entry 1 model'), 'nano', 'openrouter/openai/gpt-5-nano')
    expect(screen.queryByRole('button', { name: '+ Hyperparameters' })).toBeNull()
    expect(screen.getByText('3 executors, 1 researcher')).toBeTruthy()
    await create()
    await waitFor(() => expect(onSaved).toHaveBeenCalled())
    expect(sent()).toMatchObject({
      coordinator_model: 'anthropic/claude-sonnet-5',
      agents: [{ agent_type: 'generalist', count: 2, model: 'openrouter/openai/gpt-5-nano' }, { agent_type: 'researcher', count: 1 }, { agent_type: 'data-search', count: 1 }],
    })
  })

  it('counts the budget in H100-hours and links to the rates', async () => {
    show()
    await userEvent.click(screen.getByRole('tab', { name: '3. Budget and rules' }))
    expect(await screen.findByLabelText('Budget (H100-hours)')).toBeTruthy()
    expect(screen.getByRole('img', { name: 'Budgets are counted in H100-hours. An hour on another type counts at its rate. Jobs on CPU count CPU core-hours. Memory and storage spend nothing.' })).toBeTruthy()
    expect(screen.getByRole('link', { name: 'Rates' })).toBeTruthy()
  })

  it('creates nothing without an executor in the team', async () => {
    show()
    await userEvent.type(name(), 'Run')
    await userEvent.type(screen.getByLabelText('Experiment goal'), 'The brief')
    await next()
    await next()
    await userEvent.click(await screen.findByRole('button', { name: 'Remove team entry 1' }))
    await create()
    expect(await screen.findByText('Add at least one executor to the team.')).toBeTruthy()
    expect(createPlatformExperiment).not.toHaveBeenCalled()
  })

  it('offers the agent types a team names from the API, by role, each with what it does, and links to manage them', async () => {
    show()
    await userEvent.type(name(), 'Run')
    await userEvent.type(screen.getByLabelText('Experiment goal'), 'The brief')
    await next()
    await next()
    const type = await screen.findByLabelText('Team entry 1 type')
    expect(screen.getByText(/No fixed axis/)).toBeTruthy()
    expect(within(screen.getByRole('listitem', { name: 'Team entry 2' })).getByText('Researcher', { selector: 'span' })).toBeTruthy()
    await userEvent.click(type)
    expect(screen.getAllByRole('group').map(g => g.getAttribute('aria-label'))).toEqual(expect.arrayContaining(['Executor', 'Researcher']))
    const offered = (await screen.findAllByRole('option')).map(o => o.textContent)
    expect(offered).toEqual(['Generalist', 'Hyperparameter search', 'Architecture search', 'Data search', 'LR sweeper (v3)', 'Researcher', 'Manage agent types'])
    await userEvent.click(screen.getByRole('option', { name: 'LR sweeper (v3)' }))
    expect(screen.getByText("Your org's own executor type, made by Ada.")).toBeTruthy()
    expect(screen.getByRole('link', { name: 'Manage agent types' }).getAttribute('href')).toBe('/orgs/org_a/settings/agent-types')
    await create()
    await waitFor(() => expect(onSaved).toHaveBeenCalled())
    expect(sent()).toMatchObject({ agents: [{ agent_type: 'at-lr', count: 2 }, { agent_type: 'researcher', count: 1 }] })
  })

  it('adds a common metric or a typed key from one control, a typed key maximizing', async () => {
    show()
    await userEvent.type(name(), 'Run')
    await userEvent.type(screen.getByLabelText('Experiment goal'), 'The brief')
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
    const unbound = 'metrics: diverged is a constraint and states no bound'
    vi.mocked(createPlatformExperiment).mockRejectedValueOnce(new ApiError(422, unbound, 'invalid_metrics', {
      reason: 'invalid_metrics', message: unbound, problems: [{ reason: 'invalid_metrics', message: unbound, field: 'metrics' }],
    }))
    show()
    await userEvent.type(name(), 'Run')
    await userEvent.type(screen.getByLabelText('Experiment goal'), 'The brief')
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
    // The API refuses the gate with no bound, and the form shows why at the step of metrics.
    expect(await screen.findByText(unbound)).toBeTruthy()
    expect(screen.getByRole('tab', { name: '2. Measure', selected: true })).toBeTruthy()
    expect(sent().metrics).toContainEqual({ key: 'diverged', direction: 'maximize', role: 'constraint' })
    await userEvent.type(screen.getByLabelText('Bound of diverged'), '0')
    await userEvent.click(screen.getByRole('button', { name: 'Try again' }))
    await waitFor(() => expect(onSaved).toHaveBeenCalled())
    expect(sent(1).metrics).toEqual([
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
    expect(screen.getByLabelText('Experiment goal')).toBeTruthy()
    expect(screen.getByText('Not created. Change what the refusal names and try again.')).toBeTruthy()

    await userEvent.click(screen.getByRole('button', { name: 'Try again' }))
    await waitFor(() => expect(onSaved).toHaveBeenCalledExactlyOnceWith(created, true))
    expect(deleteExperimentDraft).toHaveBeenCalledExactlyOnceWith('prj_1', 'draft-1')
  })

})

describe('a stage ladder the API refuses', () => {
  // The form checked the ladder with a copy of the server's rules; the API is the one check now,
  // and it names every problem at once.
  it('shows each problem the API names, at the step of the first, and keeps them in the draft', async () => {
    const problems = [
      { reason: 'invalid_stages', message: 'stages: the lengths add up to 90 and must add up to 100', field: 'stages' },
      { reason: 'invalid_agents', message: 'max_agents: at most 500 executors', field: 'max_agents' },
    ]
    vi.mocked(createPlatformExperiment).mockRejectedValueOnce(new ApiError(422, `2 problems: (1) ${problems[0].message}; (2) ${problems[1].message}`, 'invalid_stages', {
      reason: 'invalid_stages', message: `2 problems: (1) ${problems[0].message}; (2) ${problems[1].message}`, problems,
    }))
    show()
    await userEvent.type(name(), 'Run')
    await userEvent.type(screen.getByLabelText('Experiment goal'), 'The brief')
    await userEvent.click(screen.getByRole('tab', { name: '3. Budget and rules' }))
    await create()
    const listed = await screen.findAllByRole('listitem')
    expect(listed.map(li => li.textContent)).toEqual(expect.arrayContaining(problems.map(p => p.message)))
    expect(screen.getByRole('tab', { name: '3. Budget and rules', selected: true })).toBeTruthy()
    await waitFor(() => expect(createExperimentDraft).toHaveBeenCalledTimes(1))
    expect(vi.mocked(createExperimentDraft).mock.calls[0][1]).toMatchObject({ step: '3', failure: problems.map(p => p.message).join(' ') })
    expect(screen.getByRole('button', { name: 'Try again' })).toBeTruthy()
  })
})

describe('a model the API does not serve', () => {
  it('names the entry and the model sent, which the API does not repeat', async () => {
    const message = 'agents[0].model is not served: the model is not one GET /models lists: openrouter serves no model of that id that this platform runs an agent on'
    vi.mocked(createPlatformExperiment).mockRejectedValueOnce(new ApiError(422, message, 'model_not_served', {
      reason: 'model_not_served', message, problems: [{ reason: 'model_not_served', message, field: 'agents[0].model' }],
    }))
    show()
    await userEvent.type(name(), 'Run')
    await userEvent.type(screen.getByLabelText('Experiment goal'), 'The brief')
    await userEvent.click(screen.getByRole('tab', { name: '3. Budget and rules' }))
    await pickModel(screen.getByLabelText('Team entry 1 model'), 'nano', 'openrouter/openai/gpt-5-nano')
    await create()
    expect(await screen.findByText(/^agents\[0\]\.model openrouter\/openai\/gpt-5-nano is not served: the model is not one/)).toBeTruthy()
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
      agents: [{ agent_type: 'data-search', count: 3, hyperparameters: { direction: 'dictionaries' } }],
    })
    show()
    await chooseRepository('acme/widgets')
    await waitFor(() => expect(name()).toHaveProperty('value', 'Widget compression'))
    expect(fetchExperimentFile).toHaveBeenCalledExactlyOnceWith('prj_1', 'acme/widgets', 'main')
    await next()
    await next()
    await create()
    await waitFor(() => expect(onSaved).toHaveBeenCalled())
    // The team's executors are the max at work, whatever max the file states: the API counts them.
    expect(sent()).toMatchObject({
      name: 'Widget compression', description: 'Make widgets smaller.', report_interval_seconds: 10, repository: 'acme/widgets', branch: 'main',
      budget_accelerator_hours: 3, duration_hours: 6,
      metrics: [{ key: 'compression_ratio', direction: 'maximize', role: 'ranking' }], stages: [{ length_pct: 100, evict_pct: 0, max_job_hours: 0 }],
      agents: [{ agent_type: 'data-search', count: 3, hyperparameters: { direction: 'dictionaries' } }],
    })
    expect(sent()).not.toHaveProperty('max_agents')
  })

  it('keeps a name already typed when the repository has no experiment file', async () => {
    show()
    await userEvent.type(name(), 'Mine')
    await chooseRepository('acme/widgets')
    await waitFor(() => expect(fetchExperimentFile).toHaveBeenCalled())
    expect(name()).toHaveProperty('value', 'Mine')
  })

  // A repository made from the experiment template and not yet filled in showed a YAML type
  // error for budget_accelerator_hours as though the file were broken.
  it('fills nothing and shows no error for an experiment file the template left unfilled', async () => {
    vi.mocked(fetchExperimentFile).mockReset().mockRejectedValue(new ApiError(422, '.hypothesisloop/experiment.yaml at main of acme/widgets still has 7 REPLACE-WITH to fill in, as the experiment template leaves it', 'experiment_file_unfilled'))
    show()
    await userEvent.type(name(), 'Mine')
    await chooseRepository('acme/widgets')
    expect(await screen.findByText(".hypothesisloop/experiment.yaml of acme/widgets at main is the template's, not yet filled in.")).toBeTruthy()
    expect(screen.queryByRole('alert')).toBeNull()
    expect(screen.queryByRole('button', { name: 'Try again' })).toBeNull()
    expect(document.body.textContent).not.toContain('REPLACE-WITH')
    expect(name()).toHaveProperty('value', 'Mine')
    expect(screen.getByLabelText('Experiment goal')).toHaveProperty('value', '')
  })

  it('says why its experiment file cannot be used once, in the API\'s words, which name the file and where it was read', async () => {
    const unusable = '.hypothesisloop/experiment.yaml of acme/widgets at main: metrics: val_loss is declared twice'
    vi.mocked(fetchExperimentFile).mockReset().mockRejectedValue(new ApiError(422, unusable, 'invalid_experiment_file'))
    show()
    await chooseRepository('acme/widgets')
    const alert = await screen.findByRole('alert')
    expect(alert.textContent).toBe(unusable)
    expect(screen.getAllByText(unusable)).toHaveLength(1)
    expect(document.body.textContent).not.toContain('Could not read')
    vi.mocked(fetchExperimentFile).mockRejectedValue(new ApiError(422, 'the repository has no experiment file', 'no_experiment_file'))
    await userEvent.click(screen.getByRole('button', { name: 'Try again' }))
    expect(await screen.findByText('acme/widgets at main has no .hypothesisloop/experiment.yaml.')).toBeTruthy()
    expect(screen.queryByRole('alert')).toBeNull()
  })
})

describe('editing a platform experiment', () => {
  const existing = { id: 'pe-1', project_id: 'prj_1', name: 'Old', description: 'the brief', status: 'open', budget_accelerator_hours: 10, max_agents: 2, starts_at: at, duration_hours: 12, stages: [], shared: false,
    editable: ['name', 'description', 'budget_accelerator_hours', 'agents', 'max_concurrent_accelerators', 'metrics', 'report_interval_seconds', 'starts_at', 'duration_hours', 'baseline_job_id', 'baseline_reason'] } as unknown as PlatformExperiment

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

  it('shows no team and no max executors to change: the team is changed in place on the experiment page', async () => {
    show(existing)
    await userEvent.click(screen.getByRole('tab', { name: '3. Budget and rules' }))
    expect(screen.queryByText('Max executors')).toBeNull()
    expect(screen.queryByRole('list', { name: 'Team' })).toBeNull()
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

// A plan made from a sentence states its machine, which the person changes while the baseline is
// measured and which is fixed once the run starts.
describe('the machine of a plan', () => {
  const planned = {
    id: 'pe-1', project_id: 'prj_1', name: 'Old', description: 'the brief', status: 'open', budget_accelerator_hours: 10, max_agents: 2, starts_at: at, duration_hours: 12, stages: [], shared: false,
    proposal: { accelerator_type: 'hypothesisloop.dev/cpu=core', cpu: '8', memory: '32Gi', storage: '100Gi' },
    editable: ['name', 'description', 'budget_accelerator_hours', 'duration_hours', 'accelerator_type', 'cpu', 'memory', 'storage'],
  } as unknown as PlatformExperiment

  it('sends what of it changed', async () => {
    vi.mocked(updatePlatformExperiment).mockResolvedValue(planned)
    show(planned)
    await userEvent.click(screen.getByRole('tab', { name: '3. Budget and rules' }))
    const cpu = screen.getByLabelText('CPU')
    expect(cpu).toHaveProperty('value', '8')
    await userEvent.clear(cpu)
    await userEvent.type(cpu, '4')
    await userEvent.click(screen.getByRole('button', { name: 'Save changes' }))
    await waitFor(() => expect(updatePlatformExperiment).toHaveBeenCalledExactlyOnceWith('pe-1', { cpu: '4' }))
  })

  it('is fixed once running', async () => {
    show({ ...planned, status: 'running', editable: ['name', 'description', 'duration_hours'] } as PlatformExperiment)
    await userEvent.click(screen.getByRole('tab', { name: '3. Budget and rules' }))
    expect(screen.getByLabelText('CPU')).toHaveProperty('disabled', true)
    expect(screen.getAllByText('Fixed once running.').length).toBeGreaterThan(0)
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
    await userEvent.type(screen.getByLabelText('Experiment goal'), 'The brief')
    await next()
    await userEvent.click(screen.getByRole('button', { name: 'Save draft' }))
    expect(await screen.findByRole('button', { name: 'Draft saved' })).toBeTruthy()
    expect(vi.mocked(createExperimentDraft).mock.calls[0]).toEqual(['prj_1', { form: expect.objectContaining({ name: 'Run', budget_accelerator_hours: '10' }), step: '2', failure: '' }])
    await next()
    // Nothing changed since it was saved, so there is nothing to save.
    expect(screen.getByRole('button', { name: 'Draft saved' })).toHaveProperty('disabled', true)
    await userEvent.type(await screen.findByLabelText('Team entry 1 count'), '0')
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

  it('is left as it is while GitHub is connected in another tab', async () => {
    const opened = { location: { href: '' }, close: vi.fn() }
    vi.spyOn(window, 'open').mockReturnValue(opened as unknown as Window)
    vi.mocked(beginGitHubInstall).mockResolvedValue({ state: 's-1', install_url: 'https://github.test/install?state=s-1', expires_at: '' })
    vi.mocked(fetchGitHubInstallations).mockResolvedValue({ items: [], total: 0 })
    show()
    await userEvent.type(name(), 'Before GitHub')
    await userEvent.click(await screen.findByRole('button', { name: 'Connect GitHub' }))
    expect(await screen.findByText(/Waiting for GitHub/)).toBeTruthy()
    expect(opened.location.href).toBe('https://github.test/install?state=s-1')
    expect(createExperimentDraft).not.toHaveBeenCalled()
    expect(name()).toHaveProperty('value', 'Before GitHub')
  })
})

describe('what a new platform experiment lacks', () => {
  it('starts from a new private repository from the template, and asks for a branch only of an existing one', async () => {
    show()
    expect(await screen.findByText('A new repository starts from the experiment template.')).toBeTruthy()
    expect(screen.queryByLabelText('Branch')).toBeNull()
    await chooseRepository('acme/widgets')
    expect(screen.getByLabelText('Branch')).toBeTruthy()
    expect(screen.queryByText('A new repository starts from the experiment template.')).toBeNull()
  })

  it('stays on step 1 at Next while the name or the goal is empty, says which, and clears each once given', async () => {
    show()
    await next()
    expect(screen.getByRole('tab', { name: '1. What', selected: true })).toBeTruthy()
    expect(screen.getByText('Name the experiment.')).toBeTruthy()
    expect(screen.getByText('Describe what the experiment should achieve.')).toBeTruthy()
    await waitFor(() => expect(document.activeElement).toBe(name()))
    await userEvent.type(name(), 'Run')
    expect(screen.queryByText('Name the experiment.')).toBeNull()
    await userEvent.type(screen.getByLabelText('Experiment goal'), 'The brief')
    await next()
    expect(screen.getByRole('tab', { name: '2. Measure', selected: true })).toBeTruthy()
  })

  it('goes back to step 1 at Create while GitHub is not connected, and points at the way to connect it', async () => {
    vi.mocked(fetchGitHubInstallations).mockResolvedValue({ items: [], total: 0 })
    show()
    await essentials()
    const button = screen.getByRole('button', { name: 'Create experiment' })
    expect(button).toHaveProperty('disabled', false)
    await create()
    expect(screen.getByRole('tab', { name: '1. What', selected: true })).toBeTruthy()
    expect(screen.getByText('Connect GitHub first.')).toBeTruthy()
    await waitFor(() => expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Connect GitHub' })))
    expect(createPlatformExperiment).not.toHaveBeenCalled()
  })

})
