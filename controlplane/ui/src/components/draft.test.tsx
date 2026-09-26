import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { apiSend, fetchExperimentMetrics, fetchPlatformExperimentAgents, startPlatformExperiment } from '@/lib/api'
import { DraftView, lastReported } from '@/components/draft'
import { draftState } from '@/lib/planning'
import type { Draft, Proposal } from '@/lib/planning'
import { PlatformExperimentStatus } from '@/types'
import type { AgentSignup, MetricDataPoint } from '@/types'

vi.mock('next/navigation', () => ({
  useRouter: () => ({ push: vi.fn() }),
  usePathname: () => '/platform-experiments/pe-1',
  useSearchParams: () => new URLSearchParams('project=prj_1'),
}))
vi.mock('@/lib/api', () => ({
  apiSend: vi.fn(), fetchExperimentMetrics: vi.fn(), fetchPlatformExperimentAgents: vi.fn(), startPlatformExperiment: vi.fn(),
}))

const proposal: Proposal = {
  experiment: {
    name: 'Cosine or constant',
    description: 'Train the small transformer in train.py and report val_loss.',
    metrics: [
      { key: 'val_loss', direction: 'minimize' },
      { key: 'exact_match', direction: 'maximize', role: 'constraint', bound: 1 },
    ],
    report_interval_seconds: 30,
  },
  budget_accelerator_hours: 6,
  duration_hours: 4,
  agents: [{ flavor: 'hyperparameter-search' }, {}],
  baseline_job_id: 'job-base',
  accelerator_type: 'rtx-4090',
  metric_reason: 'Validation loss on held-out text is the goal itself.',
  measurement: 'bench.py scores a split the training never reads.',
  questions: ['Is 4 hours enough?'],
  risks: ['One seed only.'],
}

const point = (name: string, value: number, basis = 'raw'): MetricDataPoint =>
  ({ experiment_id: 'job-base', fraction_complete: 1, metric_name: name, metric_value: value, metric_basis: basis }) as MetricDataPoint

function draft(more: Partial<Draft> = {}): Draft {
  return {
    id: 'pe-1', project_id: 'prj_1', name: 'Does a cosine schedule beat…', status: PlatformExperimentStatus.DRAFT,
    goal: 'Does a cosine schedule beat a constant one?', ends_at: '2026-10-05T12:30:00Z', ...more,
  } as Draft
}

function show(d: Draft) {
  return render(<SWRConfig value={{ provider: () => new Map() }}><DraftView draft={d} /></SWRConfig>)
}

beforeEach(() => {
  vi.mocked(apiSend).mockReset().mockResolvedValue({})
  vi.mocked(startPlatformExperiment).mockReset().mockResolvedValue(undefined)
  vi.mocked(fetchExperimentMetrics).mockResolvedValue([point('val_loss', 2.1), point('val_loss', 1.8), point('exact_match', 1), point('val_loss', 0.2, 'scaled')])
  vi.mocked(fetchPlatformExperimentAgents).mockResolvedValue([
    { platform_experiment_id: 'pe-1', agent_id: 'ag-p', name: 'Planner', role: 'baseline', hosted: true, flavor: 'planner', signed_up_at: '', instance: { state: 'starting', restarts: 0, stopped: false } } as AgentSignup,
  ])
})

describe('a draft being planned', () => {
  it('says what the planner and its machine are doing and offers nothing to accept', async () => {
    show(draft())
    expect(screen.getByText('Does a cosine schedule beat a constant one?')).toBeTruthy()
    await waitFor(() => expect(screen.getByRole('status').textContent).toMatch(/working out how to measure this and running a baseline\. Its machine is starting/))
    expect(screen.queryByRole('button', { name: 'Accept and run' })).toBeNull()
    expect(screen.queryByRole('button', { name: 'Revise' })).toBeNull()
  })

  it('is one of three states by what the API says', () => {
    expect(draftState(draft())).toBe('planning')
    expect(draftState(draft({ proposal, proposed_at: '2026-10-05T12:10:00Z' }))).toBe('proposed')
    expect(draftState(draft({ proposal }))).toBe('revising')
  })
})

describe('a proposed draft', () => {
  const proposed = () => draft({ proposal, proposed_at: '2026-10-05T12:10:00Z' })

  it('shows what is measured with the baseline the job itself reported, and the planner\'s questions first', async () => {
    show(proposed())
    expect(await screen.findByText('val_loss, lower is better, baseline 1.8')).toBeTruthy()
    expect(screen.getByText('exact_match must stay at or above 1, baseline 1')).toBeTruthy()
    expect(screen.getByText('Is 4 hours enough?')).toBeTruthy()
    expect(screen.getByText('Validation loss on held-out text is the goal itself.')).toBeTruthy()
    expect(screen.getByRole('link', { name: 'job job-base' }).getAttribute('href')).toMatch(/\/jobs\/job-base/)
    expect(screen.queryByRole('status')).toBeNull()
  })

  it('reads the last raw value of each key', () => {
    expect(Array.from(lastReported([point('a', 1), point('a', 2), point('a', 9, 'scaled'), point('b', 3)]))).toEqual([['a', 2], ['b', 3]])
  })

  it('accepts at the cost the person states, keeps the proposed flavors, and starts it', async () => {
    show(proposed())
    const budget = screen.getByLabelText('Budget (accelerator-hours)')
    expect(budget).toHaveProperty('value', '6')
    await userEvent.clear(budget)
    await userEvent.type(budget, '3')
    const researchers = screen.getByLabelText('Researchers')
    await userEvent.clear(researchers)
    await userEvent.type(researchers, '3')
    await userEvent.click(screen.getByRole('button', { name: 'Accept and run' }))
    expect(apiSend).toHaveBeenCalledExactlyOnceWith('POST', '/platform-experiments/pe-1/accept', {
      budget_accelerator_hours: 3, duration_hours: 4, agents: [{ flavor: 'hyperparameter-search' }, {}, {}],
    })
    await waitFor(() => expect(startPlatformExperiment).toHaveBeenCalledExactlyOnceWith('pe-1'))
  })

  it('does not start what the API refused to accept, and says why', async () => {
    vi.mocked(apiSend).mockRejectedValue(new Error('agents: propose between 1 and 24 researchers, got 30'))
    show(proposed())
    await userEvent.click(screen.getByRole('button', { name: 'Accept and run' }))
    expect((await screen.findByRole('alert')).textContent).toMatch(/between 1 and 24/)
    expect(startPlatformExperiment).not.toHaveBeenCalled()
  })

  it('asks for a change in a sentence', async () => {
    show(proposed())
    const revise = screen.getByRole('button', { name: 'Revise' })
    expect(revise).toHaveProperty('disabled', true)
    await userEvent.type(screen.getByLabelText('What should be different?'), 'It must fit one RTX 4090.')
    await userEvent.click(revise)
    expect(apiSend).toHaveBeenCalledExactlyOnceWith('POST', '/platform-experiments/pe-1/revise', { message: 'It must fit one RTX 4090.' })
  })

  it('keeps the proposal readable while it is revised, with what was asked, and takes no acceptance', async () => {
    show(draft({ proposal, revisions: [{ message: 'It must fit one RTX 4090.', by: { kind: 'user', id: 'u-1', name: 'Ada' }, at: '2026-10-05T12:20:00Z' }] }))
    expect(screen.getByText('It must fit one RTX 4090.')).toBeTruthy()
    expect(screen.getByText(/Ada asked/)).toBeTruthy()
    expect(screen.getByText('Proposed experiment: Cosine or constant')).toBeTruthy()
    await waitFor(() => expect(screen.getByRole('status').textContent).toMatch(/changing its proposal/))
    expect(screen.queryByRole('button', { name: 'Accept and run' })).toBeNull()
  })

  it('shows a reader of a shared draft the proposal and no control that writes', async () => {
    render(<SWRConfig value={{ provider: () => new Map() }}><DraftView draft={proposed()} readOnly /></SWRConfig>)
    expect(screen.getByText('Proposed experiment: Cosine or constant')).toBeTruthy()
    for (const name of ['Accept and run', 'Revise', 'Abandon this draft']) expect(screen.queryByRole('button', { name })).toBeNull()
  })

  it('is abandoned by closing it', async () => {
    show(proposed())
    await userEvent.click(screen.getByRole('button', { name: 'Abandon this draft' }))
    expect(apiSend).toHaveBeenCalledExactlyOnceWith('POST', '/platform-experiments/pe-1/close')
  })
})

// A goal the planner could not plan left a draft with nothing on its page until it closed.
describe('a draft the planner declined', () => {
  const why = '"Make the model better" names nothing to measure: say which model, on what data, and what better means.'
  const declined = () => draft({ declined_reason: why, proposed_at: '2026-10-05T12:10:00Z' })

  it('shows the reason, nothing to accept, and the ways to answer it or drop it', async () => {
    expect(draftState(declined())).toBe('declined')
    show(declined())
    expect(screen.getByText('The planner proposed no experiment')).toBeTruthy()
    expect(screen.getByText(why)).toBeTruthy()
    expect(screen.getByText(/Draft, waiting for you/)).toBeTruthy()
    expect(screen.queryByRole('status')).toBeNull()
    expect(screen.queryByRole('button', { name: 'Accept and run' })).toBeNull()
    expect(screen.getByRole('button', { name: 'Abandon this draft' })).toBeTruthy()
    await userEvent.type(screen.getByLabelText('What was missing?'), 'The small transformer, by validation loss.')
    await userEvent.click(screen.getByRole('button', { name: 'Revise' }))
    expect(apiSend).toHaveBeenCalledExactlyOnceWith('POST', '/platform-experiments/pe-1/revise', { message: 'The small transformer, by validation loss.' })
  })

  it('keeps the reason readable while the planner tries again', async () => {
    const retrying = draft({ declined_reason: why })
    expect(draftState(retrying)).toBe('revising')
    show(retrying)
    expect(screen.getByText(why)).toBeTruthy()
    await waitFor(() => expect(screen.getByRole('status').textContent).toMatch(/changing its proposal/))
    expect(screen.queryByRole('button', { name: 'Revise' })).toBeNull()
  })

  it('is abandoned like any draft', async () => {
    show(declined())
    await userEvent.click(screen.getByRole('button', { name: 'Abandon this draft' }))
    expect(apiSend).toHaveBeenCalledExactlyOnceWith('POST', '/platform-experiments/pe-1/close')
  })
})
