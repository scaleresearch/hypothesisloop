import { cleanup, render, screen, waitFor } from '@testing-library/react'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import * as api from '@/lib/api'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import PlatformExperimentDetailPage from '@/app/(app)/platform-experiments/[id]/page'
import { PlatformExperimentStatus } from '@/types'
import type { Me, PlatformExperiment } from '@/types'

const navigation = vi.hoisted(() => ({ search: '', push: vi.fn(), replace: vi.fn() }))

vi.mock('next/navigation', () => ({
  useRouter: () => navigation,
  usePathname: () => '/platform-experiments/pe-x',
  useSearchParams: () => new URLSearchParams(navigation.search),
}))
vi.mock('@/lib/api', () => ({
  fetchPlatformExperiment: vi.fn(), fetchPlatformExperimentQuotas: vi.fn(), fetchPlatformExperimentResults: vi.fn(),
  fetchExperimentsPage: vi.fn(), fetchExperimentStats: vi.fn(), fetchStages: vi.fn(),
  fetchExperimentMetrics: vi.fn(), fetchPlatformExperimentTimeseries: vi.fn(), fetchHypothesisStats: vi.fn(),
  fetchFindings: vi.fn(),
  addPlatformExperimentAgents: vi.fn(), fetchPlatformExperimentAgents: vi.fn(),
  fetchPlatformExperimentAgentToken: vi.fn(), removePlatformExperimentAgent: vi.fn(),
  MAX_LIST_PAGE_SIZE: 200,
  HYPOTHESIS_STATUSES: ['open', 'confirmed', 'refuted', 'inconclusive'],
}))

const at = '2026-01-01T00:00:00Z'
const me: Me = {
  kind: 'user',
  user_id: 'u1',
  orgs: [{ id: 'org_a', name: 'Ada', role: 'owner', created_at: at }],
  projects: [{ id: 'prj_1', org_id: 'org_a', name: 'default', created_at: at }],
}

function show(search: string, more: Partial<PlatformExperiment>) {
  navigation.search = search
  const status = more.status ?? PlatformExperimentStatus.OPEN
  const reason = { draft: 'planning', open: 'baseline_required', running: 'running', closed: more.close_reason ?? 'closed' }[status]
  vi.mocked(api.fetchPlatformExperiment).mockResolvedValue({
    id: 'pe-x', name: 'Vision benchmark', project_id: 'prj_platform', status, shared: true,
    status_reason: { reason, message: `the API's sentence for ${reason}`, actor: 'platform' },
    budget_accelerator_hours: 100, max_agents: 4, signup_count: 1, stages: [], current_stage: 1, created_at: at, ...more,
  })
  const resolved = resolveTenant(me, '')
  if (resolved.state !== 'selected') throw new Error('no tenant')
  return render(
    <SWRConfig value={{ provider: () => new Map() }}>
      <TenantContext.Provider value={resolved.tenant}><PlatformExperimentDetailPage params={{ id: 'pe-x' }} /></TenantContext.Provider>
    </SWRConfig>,
  )
}

beforeEach(() => {
  vi.mocked(api.fetchPlatformExperimentQuotas).mockResolvedValue([])
  vi.mocked(api.fetchPlatformExperimentResults).mockResolvedValue({ metrics: [], bests: [] })
  vi.mocked(api.fetchExperimentsPage).mockResolvedValue({ items: [], total: 0 })
  vi.mocked(api.fetchExperimentStats).mockResolvedValue({ total: 0, by_status: {} } as never)
  vi.mocked(api.fetchStages).mockResolvedValue({ stages: [], current_stage: 1, progress: 0, next_boundary_progress: 1, advances: [], active_agents: [], cut_agents: [] })
  vi.mocked(api.fetchHypothesisStats).mockResolvedValue({ total: 0, by_status: {} })
  vi.mocked(api.fetchFindings).mockResolvedValue([])
  vi.mocked(api.fetchPlatformExperimentAgents).mockResolvedValue([
    { platform_experiment_id: 'pe-x', agent_id: 'ag-self', name: 'ag-self', role: 'competitor', actor: { kind: 'user', id: 'u1', name: 'Ada' }, hosted: false, signed_up_at: at },
  ])
})

describe('a shared platform experiment of another org', () => {
  it('opens in the reader\'s own project, with one note that it is shared and read only', async () => {
    show('project=prj_1', {})
    expect(await screen.findByRole('heading', { name: 'Vision benchmark' })).toBeTruthy()
    expect(screen.getByText('Shared · read only')).toBeTruthy()
    expect(screen.getAllByRole('note')).toHaveLength(1)
    expect(screen.getByRole('note').textContent).toMatch(/shares this experiment with every account.*change nothing/)
    // The URL keeps the reader's project: the experiment's is not theirs to be in.
    expect(navigation.replace).not.toHaveBeenCalled()
  })

  it('says why a closed one closed: its end time, or who closed it early', async () => {
    const closed = { status: PlatformExperimentStatus.CLOSED, closed_at: at }
    const first = show('project=prj_1', { ...closed, close_reason: 'end_time_reached' })
    expect(await screen.findByText(/It reached its end time\.$/)).toBeTruthy()
    first.unmount()
    show('project=prj_1', { ...closed, close_reason: 'closed_by_request', closed_by: { kind: 'user', id: 'u2', name: 'Grace' } })
    expect(await screen.findByText(/It was closed early by Grace\.$/)).toBeTruthy()
    expect(screen.queryByText(/reached its end time/)).toBeNull()
    cleanup()
    show('project=prj_1', { ...closed, close_reason: 'planning_limit_reached' })
    expect(await screen.findByText(/its planner proposed nothing within the planning limit/)).toBeTruthy()
  })

  // The coordinator's closing summary was filed and shown nowhere.
  it('shows the coordinator\'s summary of a run, and no card for one that has none', async () => {
    const first = show('project=prj_1', { status: PlatformExperimentStatus.CLOSED, close_reason: 'end_time_reached', summary: 'Cosine won by 0.04 val_loss.\nWarmup mattered more than decay.' })
    const card = (await screen.findByRole('heading', { name: 'The coordinator\'s summary' })).parentElement!.parentElement!
    expect(card.querySelector('p')!.textContent).toBe('Cosine won by 0.04 val_loss.\nWarmup mattered more than decay.')
    first.unmount()
    cleanup()
    show('project=prj_1', { status: PlatformExperimentStatus.CLOSED, close_reason: 'end_time_reached' })
    await screen.findByRole('heading', { name: 'Vision benchmark' })
    expect(screen.queryByRole('heading', { name: 'The coordinator\'s summary' })).toBeNull()
  })

  it('shows its agents and no control that writes', async () => {
    show('project=prj_1', {})
    expect(await screen.findByRole('cell', { name: 'ag-self' })).toBeTruthy()
    for (const control of ['Add agent', 'Show token', 'Remove']) {
      expect(screen.queryByRole('button', { name: control })).toBeNull()
    }
  })

  it('reads its lists within the experiment\'s own project, never the reader\'s', async () => {
    show('project=prj_1', {})
    await screen.findByRole('heading', { name: 'Vision benchmark' })
    for (const read of [api.fetchExperimentsPage, api.fetchExperimentStats]) {
      await vi.waitFor(() => expect(read).toHaveBeenCalled())
      for (const [params] of vi.mocked(read).mock.calls) {
        expect(params).toMatchObject({ project_id: 'prj_platform', platform_experiment_id: 'pe-x' })
      }
    }
  })
})

// An accepted experiment showed an empty page for as long as its planner's baseline had taken
// before. No experiment starts without a baseline now, and its page says which side of that it is.
describe('the baseline of a platform experiment', () => {
  it('is being prepared while the experiment is open without one, and is the job with what it reported once declared', async () => {
    const first = show('project=prj_1', {})
    expect(await screen.findByText(/Preparing the baseline\. No researcher starts/)).toBeTruthy()
    first.unmount()
    cleanup()

    const point = (name: string, value: number, basis = 'raw') => ({ experiment_id: 'job-base', fraction_complete: 1, metric_name: name, metric_value: value, metric_basis: basis })
    vi.mocked(api.fetchPlatformExperimentTimeseries).mockResolvedValue({ series: [] })
    vi.mocked(api.fetchExperimentMetrics).mockResolvedValue([point('val_loss', 2.1), point('val_loss', 1.8), point('exact_match', 1), point('val_loss', 0.2, 'scaled')] as never)
    show('project=prj_1', {
      status: PlatformExperimentStatus.RUNNING, baseline_job_id: 'job-base',
      metrics: [{ key: 'val_loss', direction: 'minimize' }, { key: 'exact_match', direction: 'maximize', role: 'constraint', bound: 1 }],
    })
    const link = await screen.findByRole('link', { name: 'job job-base' })
    expect(link.getAttribute('href')).toMatch(/\/jobs\/job-base/)
    await waitFor(() => expect(link.parentElement!.textContent).toBe('Baseline measured by job job-base: val_loss 1.8, exact_match 1.'))
    expect(screen.queryByText(/Preparing the baseline/)).toBeNull()
  })

  it('is not asked for on a running or closed experiment that never had one, and a close for want of one says so', async () => {
    const first = show('project=prj_1', { status: PlatformExperimentStatus.RUNNING })
    await screen.findByRole('heading', { name: 'Vision benchmark' })
    expect(screen.queryByText(/Preparing the baseline/)).toBeNull()
    first.unmount()
    cleanup()
    show('project=prj_1', { status: PlatformExperimentStatus.CLOSED, close_reason: 'baseline_limit_reached' })
    expect(await screen.findByText(/It never ran: no baseline was measured in time\.$/)).toBeTruthy()
  })
})

// The minutes before a run showed a scoreboard of nobody and charts of nothing above what was
// actually happening.
describe('the page of an experiment that has not started', () => {
  const metrics = [{ key: 'val_loss', direction: 'minimize' as const }]

  it('says where it stands at the top and shows no scoreboard and no chart until the run starts', async () => {
    vi.mocked(api.fetchPlatformExperimentTimeseries).mockResolvedValue({ series: [] })
    const first = show('project=prj_1', { metrics })
    expect((await screen.findByRole('region', { name: 'Where this experiment stands' })).textContent).toContain('the API\'s sentence for baseline_required')
    expect(screen.getByRole('heading', { name: /^Agents/ })).toBeTruthy()
    expect(screen.getByRole('heading', { name: 'Jobs' })).toBeTruthy()
    for (const hidden of ['Scoreboard', 'Agents over time']) expect(screen.queryByRole('heading', { name: hidden })).toBeNull()
    first.unmount()
    cleanup()
    show('project=prj_1', { status: PlatformExperimentStatus.RUNNING, metrics, ends_at: at })
    expect(await screen.findByRole('heading', { name: 'Scoreboard' })).toBeTruthy()
    expect(screen.getByRole('heading', { name: 'Agents over time' })).toBeTruthy()
  })
})

// The baseline's job tests no hypothesis, and its row read as an agent with no best job.
describe('the scoreboard row of the baseline agent', () => {
  it('says its best job measured the objective, in place of a hypothesis', async () => {
    vi.mocked(api.fetchPlatformExperimentTimeseries).mockResolvedValue({ series: [] })
    vi.mocked(api.fetchPlatformExperimentQuotas).mockResolvedValue([{ agent_id: 'ag-base' }, { agent_id: 'ag-other' }] as never)
    vi.mocked(api.fetchPlatformExperimentResults).mockResolvedValue({
      metrics: [{ metric: 'val_loss', direction: 'minimize', standings: [{ rank: 1, agent_id: 'ag-base', best: 1.8, experiment_id: 'job-base', objective: true }] }],
      bests: [],
    })
    show('project=prj_1', { status: PlatformExperimentStatus.RUNNING, ends_at: at, metrics: [{ key: 'val_loss', direction: 'minimize' }] })
    const said = await screen.findByText('The baseline: it measures the objective and tests no hypothesis')
    const scoreboard = said.closest('table')!
    expect(said.closest('tr')!.textContent).toMatch(/^#1/)
    expect(scoreboard.querySelectorAll('tbody tr')).toHaveLength(2)
    expect(scoreboard.querySelectorAll('tbody tr')[1].textContent).not.toContain('The baseline')
  })
})

describe('a shared platform experiment of the caller\'s own org', () => {
  it('is theirs to change, marked shared, with no read-only note, and puts its project in the URL', async () => {
    show('', { project_id: 'prj_1' })
    expect(await screen.findByRole('button', { name: 'Add agent' })).toBeTruthy()
    expect(await screen.findByRole('button', { name: 'Show token' })).toBeTruthy()
    expect(screen.getByText('Shared')).toBeTruthy()
    expect(screen.queryByRole('note')).toBeNull()
    expect(navigation.replace).toHaveBeenCalledWith('/platform-experiments/pe-x?project=prj_1')
  })
})

describe('a platform experiment nobody shared', () => {
  it('carries no mark', async () => {
    show('project=prj_1', { project_id: 'prj_1', shared: false })
    expect(await screen.findByRole('button', { name: 'Add agent' })).toBeTruthy()
    expect(screen.queryByText(/Shared/)).toBeNull()
  })
})
