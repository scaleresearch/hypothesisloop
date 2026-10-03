import { render, screen, within } from '@testing-library/react'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { fetchPlatformExperimentAgents } from '@/lib/api'
import { PhaseStrip } from '@/components/phase-strip'
import { formatDate } from '@/lib/format'
import type { Draft } from '@/lib/planning'
import { ExperimentStatus, PlatformExperimentStatus } from '@/types'
import type { Experiment, JoinedAgent, StatusReason } from '@/types'

vi.mock('next/navigation', () => ({
  useRouter: () => ({ push: vi.fn() }),
  usePathname: () => '/platform-experiments/pe-1',
  useSearchParams: () => new URLSearchParams('project=prj_1'),
}))
vi.mock('@/lib/api', () => ({ fetchPlatformExperimentAgents: vi.fn() }))

const at = '2026-10-05T12:30:00Z'

function experiment(status: `${PlatformExperimentStatus}`, reason: string, more: Partial<Draft> = {}, said: Partial<StatusReason> = {}): Draft {
  return {
    id: 'pe-1', project_id: 'prj_1', name: 'Cosine or constant', status,
    status_reason: { reason, message: `the API's sentence for ${reason}`, actor: 'platform', ...said }, ...more,
  } as Draft
}

function agent(role: JoinedAgent['role'], more: Partial<JoinedAgent> = {}): JoinedAgent {
  return {
    platform_experiment_id: 'pe-1', agent_id: `ag-${role}`, name: role === 'baseline' ? 'Baseline 1' : 'Planner 1', role, hosted: true, flavor: role === 'planner' ? 'planner' : 'generalist',
    actor: { kind: 'coordinator', id: 'pe-1', name: 'coordinator' }, signed_up_at: at, instance: { state: 'running', restarts: 0, stopped: false }, ...more,
  }
}

const job = (more: Partial<Experiment>) => ({ id: 'job-base', agent_id: 'ag-baseline', status: ExperimentStatus.RUNNING, ...more }) as Experiment

function show(pe: Draft, jobs: Experiment[] = []) {
  return render(<SWRConfig value={{ provider: () => new Map() }}><PhaseStrip pe={pe} jobs={jobs} /></SWRConfig>)
}

const phases = () => within(screen.getByRole('list', { name: 'Phase' })).getAllByRole('listitem')
const current = () => phases().filter(li => li.getAttribute('aria-current') === 'step').map(li => li.textContent)
const sentence = () => screen.getByRole('status').textContent

beforeEach(() => {
  vi.mocked(fetchPlatformExperimentAgents).mockResolvedValue([])
})

// A page said where its experiment stood in a different place and in different words in each
// state, and in none of them what came before or after.
describe('the phase strip', () => {
  it('names the five phases in order, in every state', () => {
    show(experiment('open', 'starting'))
    expect(phases().map(li => li.textContent)).toEqual(['Planning', 'Proposal', 'Preparing baseline', 'Running', 'Closed'])
  })

  it.each([
    ['draft', 'planning', 'Planning'],
    ['draft', 'revising', 'Planning'],
    ['draft', 'proposed', 'Proposal'],
    ['draft', 'declined', 'Proposal'],
    ['open', 'baseline_required', 'Preparing baseline'],
    ['open', 'starts_at_ahead', 'Preparing baseline'],
    ['open', 'starting', 'Preparing baseline'],
    ['running', 'agents_starting', 'Running'],
    ['running', 'running', 'Running'],
    ['closed', 'end_time_reached', 'Closed'],
    ['closed', 'agents_not_started', 'Closed'],
  ] as const)('stands at one phase, from the status and its reason alone: %s, %s', (status, reason, phase) => {
    show(experiment(status, reason))
    expect(current()).toEqual([phase])
  })

  it('refuses an experiment that came without the reason every read of one carries', () => {
    vi.spyOn(console, 'error').mockImplementation(() => {})
    expect(() => show({ ...experiment('draft', 'planning'), status_reason: undefined })).toThrow(/came without a status_reason/)
  })

  it('says a planner is at work, with what it says it is doing, while a draft is planned', async () => {
    vi.mocked(fetchPlatformExperimentAgents).mockResolvedValue([agent('planner', { activity: { text: 'Sizing a trial on rtx-4090.', at: new Date().toISOString() } })])
    show(experiment('draft', 'planning', { planning_ends_at: at }))
    expect(sentence()).toMatch(/^The planner is working out how to measure this\. It has until/)
    expect(await screen.findByText('Sizing a trial on rtx-4090.')).toBeTruthy()
  })

  it('says in the API\'s words why an open experiment is not running, and that its baseline is being prepared while it has none', () => {
    const first = show(experiment('open', 'baseline_required'))
    expect(sentence()).toBe('Preparing the baseline. No researcher starts and none of their machines is rented until it is measured. the API\'s sentence for baseline_required')
    first.unmount()
    show(experiment('open', 'starts_at_ahead', { baseline_job_id: 'job-base' }))
    expect(sentence()).toBe('the API\'s sentence for starts_at_ahead')
  })

  it('shows the researchers\' machines starting with the deadline while a running experiment waits for the first', () => {
    show(experiment('running', 'agents_starting', { agents_due_at: at }))
    expect(sentence()).toBe(`The researchers' machines are starting. The run's clock starts when the first one is running, and the platform closes the experiment if none is by ${formatDate(at)}.`)
  })

  it('says until when the researchers work once the run\'s clock started', () => {
    show(experiment('running', 'running', { started_at: at, ends_at: at }))
    expect(sentence()).toBe(`The researchers are at work until ${formatDate(at)}.`)
  })
})

// The baseline was one more row of the jobs table, under charts and a scoreboard with nothing in them.
describe('the baseline agent in the phase strip', () => {
  const open = () => experiment('open', 'baseline_required')
  const group = () => screen.findByRole('group', { name: 'Baseline agent' })
  const jobLine = async () => (await within(await group()).findByRole('link', { name: 'Its job' })).parentElement!.textContent

  beforeEach(() => {
    vi.mocked(fetchPlatformExperimentAgents).mockResolvedValue([
      agent('baseline', { activity: { text: 'Writing the control configuration.', at: new Date().toISOString() } }),
    ])
  })

  it('shows what it is doing and where its machine stands, and that it has no job yet', async () => {
    show(open())
    const baseline = within(await group())
    expect(baseline.getByText('Writing the control configuration.')).toBeTruthy()
    expect(baseline.getByText('Its machine is working.')).toBeTruthy()
    expect(baseline.getByText('It has submitted no job yet.')).toBeTruthy()
  })

  it.each([
    [{ status: ExperimentStatus.QUEUED, not_admitted_reason: 'no_capacity', not_admitted_message: 'no cluster has a free rtx-4090' }, 'Its job is queued: no cluster has a free rtx-4090'],
    [{ status: ExperimentStatus.QUEUED, not_admitted_reason: 'no_capacity' }, 'Its job is queued: no_capacity'],
    [{ status: ExperimentStatus.SUBMITTED }, 'Its job is queued.'],
    [{ status: ExperimentStatus.RUNNING }, 'Its job is running.'],
    [{ status: ExperimentStatus.RUNNING, metric: { name: 'val_loss', value: 1.8, basis: 'raw' } }, 'Its job is running: val_loss 1.8.'],
    [{ status: ExperimentStatus.COMPLETED, metric: { name: 'val_loss', value: 1.6, basis: 'raw' } }, 'Its job is done: val_loss 1.6. The platform declares it the baseline and starts the run once the agent finishes.'],
  ])('says where its job stands: %j', async (state, line) => {
    show(open(), [job({ id: 'job-other', agent_id: 'ag-other' }), job(state)])
    expect(await jobLine()).toBe(line)
    expect(within(await group()).getByRole('link', { name: 'Its job' }).getAttribute('href')).toBe('/jobs/job-base?project=prj_1')
  })

  it('says how a job that did not complete ended', async () => {
    show(open(), [job({ status: ExperimentStatus.FAILED, eviction_reason: 'job_failed: exit code 1' })])
    expect(await jobLine()).toMatch(/^Its job ended failed\./)
  })

  it('is not shown once the run started, and is the one still measuring when another finished', async () => {
    vi.mocked(fetchPlatformExperimentAgents).mockResolvedValue([
      agent('baseline', { agent_id: 'ag-first', name: 'Baseline 1', finished_at: at, finished_reason: 'the harness does not build' }),
      agent('baseline', { agent_id: 'ag-second', name: 'Baseline 2' }),
    ])
    const first = show(open())
    expect((await group()).textContent).toContain('Baseline 2')
    first.unmount()
    show(experiment('running', 'running', { ends_at: at }))
    expect(screen.queryByRole('group', { name: 'Baseline agent' })).toBeNull()
  })
})
