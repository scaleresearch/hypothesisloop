import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { apiSend, fetchPlatformExperimentAgents } from '@/lib/api'
import { DraftView } from '@/components/draft'
import { formatDate } from '@/lib/format'
import type { Draft, Proposal } from '@/lib/planning'
import { PlatformExperimentStatus } from '@/types'
import type { AgentSignup } from '@/types'

vi.mock('next/navigation', () => ({
  useRouter: () => ({ push: vi.fn() }),
  usePathname: () => '/platform-experiments/pe-1',
  useSearchParams: () => new URLSearchParams('project=prj_1'),
}))
vi.mock('@/lib/api', () => ({
  apiSend: vi.fn(), fetchPlatformExperimentAgents: vi.fn(),
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
  accelerator_type: 'rtx-4090',
  metric_reason: 'Validation loss on held-out text is the goal itself.',
  measurement: 'bench.py scores a split the training never reads.',
  sizing_reason: 'An estimate, nothing has run: a trial is assumed to take 6 minutes, so 30 trials each for 2 researchers is 6.',
  questions: ['Is 4 hours enough?'],
  risks: ['One seed only.'],
}

function draft(reason: 'planning' | 'revising' | 'proposed' | 'declined', more: Partial<Draft> = {}): Draft {
  return {
    id: 'pe-1', project_id: 'prj_1', name: 'Does a cosine schedule beat…', status: PlatformExperimentStatus.DRAFT,
    status_reason: { reason, message: '' },
    goal: 'Does a cosine schedule beat a constant one?', planning_ends_at: '2026-10-05T12:30:00Z', ...more,
  } as Draft
}

function show(d: Draft) {
  return render(<SWRConfig value={{ provider: () => new Map() }}><DraftView draft={d} /></SWRConfig>)
}

beforeEach(() => {
  vi.mocked(apiSend).mockReset().mockResolvedValue({})
  vi.mocked(fetchPlatformExperimentAgents).mockResolvedValue([
    { platform_experiment_id: 'pe-1', agent_id: 'ag-p', name: 'Planner', role: 'planner', hosted: true, flavor: 'planner', signed_up_at: '', instance: { state: 'starting', restarts: 0, stopped: false } } as AgentSignup,
  ])
})

describe('a draft being planned', () => {
  it('says what the planner and its machine are doing and offers nothing to accept', async () => {
    show(draft('planning'))
    expect(screen.getByText('Does a cosine schedule beat a constant one?')).toBeTruthy()
    await waitFor(() => expect(screen.getByRole('status').textContent).toMatch(/working out how to measure this\. Its machine is starting/))
    expect(screen.queryByRole('button', { name: 'Accept and run' })).toBeNull()
    expect(screen.queryByRole('button', { name: 'Revise' })).toBeNull()
  })
})

describe('a proposed draft', () => {
  const proposed = () => draft('proposed', { proposal, proposed_at: '2026-10-05T12:10:00Z' })

  // A proposal once waited for a baseline job and showed its numbers. Nothing has run when a
  // person reads one now, and the page says so in place of a number nobody measured.
  it('shows what is measured with no value, says nothing has run, and puts the planner\'s questions first', async () => {
    show(proposed())
    expect(await screen.findByText('val_loss, lower is better')).toBeTruthy()
    expect(screen.getByText('exact_match must stay at or above 1')).toBeTruthy()
    expect(screen.getByText(/Nothing has run yet\. Once you accept, the baseline is measured on rtx-4090 before any researcher starts/)).toBeTruthy()
    expect(screen.getByText(/the figures are an estimate, since nothing has run, and the budget is a cap/)).toBeTruthy()
    expect(screen.queryByText(/baseline 1/)).toBeNull()
    expect(screen.getByText('Is 4 hours enough?')).toBeTruthy()
    expect(within(screen.getByText('Why this size').parentElement!).getByText(/30 trials each for 2 researchers is 6/)).toBeTruthy()
    expect(screen.getByText('Validation loss on held-out text is the goal itself.')).toBeTruthy()
    expect(screen.queryByRole('status')).toBeNull()
  })

  // The page once started the experiment right after accepting it. The platform starts it,
  // once its baseline is declared: accepting is the one call.
  it('accepts at the cost the person states, keeps the proposed flavors, and starts nothing', async () => {
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
  })

  it('says why the API refused to accept', async () => {
    vi.mocked(apiSend).mockRejectedValue(new Error('agents: propose between 1 and 24 researchers, got 30'))
    show(proposed())
    await userEvent.click(screen.getByRole('button', { name: 'Accept and run' }))
    expect((await screen.findByRole('alert')).textContent).toMatch(/between 1 and 24/)
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
    show(draft('revising', { proposal, revisions: [{ message: 'It must fit one RTX 4090.', actor: { kind: 'user', id: 'u-1', name: 'Ada' }, at: '2026-10-05T12:20:00Z' }] }))
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
  const declined = () => draft('declined', { declined_reason: why, declined_at: '2026-10-05T12:10:00Z', planning_ends_at: undefined, proposal_expires_at: '2026-10-12T12:10:00Z' })

  it('shows the reason, nothing to accept, and the ways to answer it or drop it', async () => {
    show(declined())
    expect(screen.getByText('The planner proposed no experiment')).toBeTruthy()
    expect(screen.getByText(why)).toBeTruthy()
    expect(screen.getByText(/Draft, waiting for you/)).toBeTruthy()
    expect(screen.getByText(new RegExp(`it is closed on ${formatDate('2026-10-12T12:10:00Z')} otherwise`))).toBeTruthy()
    expect(screen.queryByRole('status')).toBeNull()
    expect(screen.queryByRole('button', { name: 'Accept and run' })).toBeNull()
    expect(screen.getByRole('button', { name: 'Abandon this draft' })).toBeTruthy()
    await userEvent.type(screen.getByLabelText('What was missing?'), 'The small transformer, by validation loss.')
    await userEvent.click(screen.getByRole('button', { name: 'Revise' }))
    expect(apiSend).toHaveBeenCalledExactlyOnceWith('POST', '/platform-experiments/pe-1/revise', { message: 'The small transformer, by validation loss.' })
  })

  it('keeps the reason readable while the planner tries again', async () => {
    const retrying = draft('revising', { declined_reason: why })
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
