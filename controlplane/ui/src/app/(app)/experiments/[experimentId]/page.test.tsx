import { cleanup, render, screen, waitFor } from '@testing-library/react'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import * as api from '@/lib/api'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import PlatformExperimentDetailPage from '@/app/(app)/experiments/[experimentId]/page'
import { PlatformExperimentStatus } from '@/types'
import type { Me, PlatformExperiment } from '@/types'

const navigation = vi.hoisted(() => ({ search: '', push: vi.fn(), replace: vi.fn() }))

vi.mock('next/navigation', () => ({
  useRouter: () => navigation,
  usePathname: () => '/experiments/pe-x',
  useSearchParams: () => new URLSearchParams(navigation.search),
}))
vi.mock('@/lib/api', () => ({
  fetchPlatformExperiment: vi.fn(), fetchPlatformExperimentQuotas: vi.fn(), fetchPlatformExperimentResults: vi.fn(),
  fetchExperimentsPage: vi.fn(), fetchExperimentStats: vi.fn(), fetchStages: vi.fn(),
  fetchExperimentMetrics: vi.fn(), fetchPlatformExperimentTimeseries: vi.fn(), fetchHypothesisStats: vi.fn(),
  fetchFindings: vi.fn(),
  addPlatformExperimentAgents: vi.fn(), fetchPlatformExperimentAgents: vi.fn(),
  fetchPlatformExperimentAgentToken: vi.fn(), removePlatformExperimentAgent: vi.fn(),
  fetchPlatformExperimentMessages: vi.fn().mockResolvedValue([]), sendPlatformExperimentMessage: vi.fn(),
  fetchAgentTypes: vi.fn().mockResolvedValue([]), updatePlatformExperiment: vi.fn(),
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
  const reason = { draft: 'planning', open: 'baseline_measuring', running: 'running', closed: more.close_reason ?? 'closed' }[status]
  vi.mocked(api.fetchPlatformExperiment).mockResolvedValue({
    id: 'pe-x', name: 'Vision benchmark', project_id: 'prj_platform', status, shared: true,
    status_reason: { reason, message: `the API's sentence for ${reason}`, summary: `The summary for ${reason}.`, ...(status === PlatformExperimentStatus.CLOSED ? {} : { waits_for: 'platform' as const }) },
    budget_accelerator_hours: 100, max_agents: 4, signup_count: 1, stages: [], current_stage: 1, created_at: at, ...more,
  })
  const resolved = resolveTenant(me, '')
  if (resolved.state !== 'selected') throw new Error('no tenant')
  return render(
    <SWRConfig value={{ provider: () => new Map() }}>
      <TenantContext.Provider value={resolved.tenant}><PlatformExperimentDetailPage params={{ experimentId: 'pe-x' }} /></TenantContext.Provider>
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
    { platform_experiment_id: 'pe-x', agent_id: 'ag-self', name: 'ag-self', role: 'competitor', actor: { kind: 'user', id: 'u1', name: 'Ada' }, hosted: false, at_work: true, signed_up_at: at },
  ])
})

describe('a shared platform experiment of another org', () => {
  it('opens in the reader\'s own project, marked shared and read only', async () => {
    show('', {})
    expect(await screen.findByRole('heading', { name: 'Vision benchmark' })).toBeTruthy()
    expect(screen.getByText('Shared · read only')).toBeTruthy()
    expect(screen.queryByRole('note')).toBeNull()
  })

  it('shows its agents and no control that writes', async () => {
    show('', {})
    expect(await screen.findByRole('cell', { name: 'ag-self' })).toBeTruthy()
    for (const control of ['Add agent', 'Show token', 'Remove']) {
      expect(screen.queryByRole('button', { name: control })).toBeNull()
    }
  })

  it('reads its lists within the experiment\'s own project, never the reader\'s', async () => {
    show('', {})
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
    const first = show('', {})
    expect(await screen.findByText(/Preparing the baseline\./)).toBeTruthy()
    first.unmount()
    cleanup()

    const point = (name: string, value: number, basis = 'raw') => ({ experiment_id: 'job-base', fraction_complete: 1, metric_name: name, metric_value: value, metric_basis: basis })
    vi.mocked(api.fetchPlatformExperimentTimeseries).mockResolvedValue({ series: [] })
    vi.mocked(api.fetchExperimentMetrics).mockResolvedValue([point('val_loss', 2.1), point('val_loss', 1.8), point('exact_match', 1), point('val_loss', 0.2, 'scaled')] as never)
    show('', {
      status: PlatformExperimentStatus.RUNNING, baseline_job_id: 'job-base',
      metrics: [{ key: 'val_loss', direction: 'minimize' }, { key: 'exact_match', direction: 'maximize', role: 'constraint', bound: 1 }],
    })
    const link = await screen.findByRole('link', { name: 'job job-base' })
    expect(link.getAttribute('href')).toBe('/experiments/pe-x/jobs/job-base')
    await waitFor(() => expect(link.parentElement!.textContent).toBe('Baseline measured by job job-base: val_loss 1.8, exact_match 1.'))
    expect(screen.queryByText(/Preparing the baseline/)).toBeNull()
  })

  it('says who raised it while the run went on, from which job and why', async () => {
    vi.mocked(api.fetchPlatformExperimentTimeseries).mockResolvedValue({ series: [] })
    vi.mocked(api.fetchExperimentMetrics).mockResolvedValue([{ experiment_id: 'job-2', fraction_complete: 1, metric_name: 'val_loss', metric_value: 1.6, metric_basis: 'raw' }] as never)
    show('', {
      status: PlatformExperimentStatus.RUNNING, baseline_job_id: 'job-2', metrics: [{ key: 'val_loss', direction: 'minimize' }],
      baseline_raises: [{ from: 'job-1', to: 'job-2', reason: "the control's other coder", detail: '', actor: { kind: 'coordinator', id: 'pe-x', name: 'Coordinator' }, at: '2026-10-08T19:00:00Z' }],
    })
    const link = await screen.findByRole('link', { name: 'job job-2' })
    await waitFor(() => expect(link.parentElement!.textContent).toBe("Baseline measured by job job-2: val_loss 1.6. Raised from job job-1 by Coordinator: the control's other coder."))
  })
})

describe('the references of a platform experiment', () => {
  it('lists every measured value with whether its job met the constraints, and the leader against the best that counts', async () => {
    vi.mocked(api.fetchPlatformExperimentResults).mockResolvedValue({
      metrics: [], bests: [],
      references: [
        {
          metric: 'other_val_accuracy', direction: 'maximize', best: 0.81, leader_metric: 'val_accuracy', leader: 0.84, beaten: true,
          values: [
            { agent_id: 'ag-self', value: 0.81, experiment_id: 'job-ref', constraints_met: 'true' },
            { agent_id: 'ag-self', value: 0.9, experiment_id: 'job-fast', constraints_met: 'false' },
          ],
        },
        { metric: 'unmeasured', direction: 'maximize', values: [] },
      ],
    })
    show('', { status: PlatformExperimentStatus.RUNNING })
    expect(await screen.findByRole('heading', { name: /^References/ })).toBeTruthy()
    expect(screen.getByText(/best that counts 0\.8100/)).toBeTruthy()
    expect(screen.getByText(/beaten by the leader's val_accuracy 0\.8400/)).toBeTruthy()
    expect(screen.getByRole('link', { name: '0.8100' }).getAttribute('href')).toBe('/experiments/pe-x/jobs/job-ref')
    expect(screen.getByText('counts')).toBeTruthy()
    expect(screen.getByText('broke a constraint')).toBeTruthy()
    expect(screen.queryByText(/unmeasured/)).toBeNull()
  })
})

describe('a shared platform experiment of the caller\'s own org', () => {
  it('is theirs to change, marked shared, with no read-only note', async () => {
    show('', { project_id: 'prj_1' })
    expect(await screen.findByRole('button', { name: 'Add agent' })).toBeTruthy()
    expect(await screen.findByRole('button', { name: 'Show token' })).toBeTruthy()
    expect(screen.getByText('Shared')).toBeTruthy()
    expect(screen.queryByRole('note')).toBeNull()
  })
})
