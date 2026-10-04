import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SWRConfig } from 'swr'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { apiSend, fetchPlatformExperimentAgents } from '@/lib/api'
import { DraftView } from '@/components/draft'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import { formatDate } from '@/lib/format'
import type { Draft, Proposal } from '@/lib/planning'
import { PlatformExperimentStatus } from '@/types'
import type { AgentSignup, Me } from '@/types'

vi.mock('next/navigation', () => ({
  useRouter: () => ({ push: vi.fn() }),
  usePathname: () => '/experiments/pe-1',
  useSearchParams: () => new URLSearchParams(),
}))
vi.mock('@/lib/api', async importActual => ({
  ApiError: (await importActual<typeof import('@/lib/api')>()).ApiError,
  apiSend: vi.fn(), fetchPlatformExperimentAgents: vi.fn(),
}))

const sweeper = { flavor: 'hyperparameter-search', model: 'claude-opus-4-5', hyperparameters: { direction: 'Find the schedule: warmup length first, then the decay.' } }
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
  baseline_minutes: 30,
  agents: [sweeper, {}],
  accelerator_type: 'rtx-4090',
  image: 'pytorch',
  metric_reason: 'Validation loss on held-out text is the goal itself.',
  measurement: 'bench.py scores a split the training never reads.',
  sizing_reason: 'An estimate, nothing has run: a trial is assumed to take 6 minutes, so 30 trials each for 2 researchers is 6.',
  questions: ['Is 4 hours enough?'],
  risks: ['One seed only.'],
}

const me: Me = {
  kind: 'user',
  user_id: 'u1',
  orgs: [{ id: 'org_a', name: 'Ada', role: 'owner', created_at: '2026-01-01T00:00:00Z' }],
  projects: [{ id: 'prj_1', org_id: 'org_a', name: 'default', created_at: '2026-01-01T00:00:00Z' }],
}
const resolved = resolveTenant(me, 'prj_1')
if (resolved.state !== 'selected') throw new Error('no tenant')
const tenant = resolved.tenant

function draft(reason: 'planning' | 'revising' | 'proposed' | 'declined', more: Partial<Draft> = {}): Draft {
  return {
    id: 'pe-1', project_id: 'prj_1', name: 'Does a cosine schedule beat…', status: PlatformExperimentStatus.DRAFT,
    status_reason: { reason, message: '', waits_for: reason === 'proposed' || reason === 'declined' ? 'owner' : 'platform' },
    goal: 'Does a cosine schedule beat a constant one?', planning_ends_at: '2026-10-05T12:30:00Z', ...more,
  } as Draft
}

function show(d: Draft) {
  return render(<SWRConfig value={{ provider: () => new Map() }}><TenantContext.Provider value={tenant}><DraftView draft={d} /></TenantContext.Provider></SWRConfig>)
}

beforeEach(() => {
  vi.mocked(apiSend).mockReset().mockResolvedValue({})
  vi.mocked(fetchPlatformExperimentAgents).mockResolvedValue([
    { platform_experiment_id: 'pe-1', agent_id: 'ag-p', name: 'Planner', role: 'planner', hosted: true, flavor: 'planner', signed_up_at: '', instance: { state: 'starting', restarts: 0, stopped: false } } as AgentSignup,
  ])
})

const phase = () => within(screen.getByRole('list', { name: 'Phase' })).getAllByRole('listitem').filter(li => li.getAttribute('aria-current') === 'step').map(li => li.textContent).join()
const machineStep = () => within(screen.getByRole('list', { name: 'Machine' })).queryAllByRole('listitem').find(li => li.getAttribute('aria-current') === 'step')?.textContent

// The page said one sentence for as long as planning took, and the same one whether the
// planner's machine was still being rented or the planner was reading the repository.
describe('a draft being planned', () => {
  it('says the phase and where the planner\'s machine stands, and offers nothing to accept', async () => {
    show(draft('planning'))
    expect(screen.getByText('Does a cosine schedule beat a constant one?')).toBeTruthy()
    expect(screen.getByRole('status').textContent).toBe('Planning your experiment.')
    expect(screen.queryByText(/Nothing is needed from you/)).toBeNull()
    expect(phase()).toBe('Planning')
    expect(await screen.findByText('Starting the planner, usually about a minute.')).toBeTruthy()
    expect(machineStep()).toBe('Starting the planner')
    expect(screen.queryByRole('button', { name: 'Accept and run' })).toBeNull()
    expect(screen.queryByRole('button', { name: 'Revise' })).toBeNull()
    expect(screen.getByRole('button', { name: 'Abandon this draft' })).toBeTruthy()
  })

  describe('reads the roster', () => {
    beforeEach(() => { vi.useFakeTimers() })
    afterEach(() => { vi.useRealTimers() })
    const reads = () => vi.mocked(fetchPlatformExperimentAgents).mock.calls.length

    it('every 3 s while the planner works and the page is visible, and not while it is hidden', async () => {
      vi.mocked(fetchPlatformExperimentAgents).mockClear()
      show(draft('planning'))
      await vi.advanceTimersByTimeAsync(100)
      expect(reads()).toBe(1)
      await vi.advanceTimersByTimeAsync(3_000)
      expect(reads()).toBe(2)
      vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('hidden')
      await vi.advanceTimersByTimeAsync(9_000)
      expect(reads()).toBe(2)
    })
  })
})

describe('a proposed draft', () => {
  const proposed = () => draft('proposed', { proposal, proposed_at: '2026-10-05T12:10:00Z' })

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
    const baseline = screen.getByLabelText('Baseline (minutes)')
    expect(baseline).toHaveProperty('value', '30')
    await userEvent.clear(baseline)
    await userEvent.type(baseline, '45')
    await userEvent.click(screen.getByRole('button', { name: 'Accept and run' }))
    expect(apiSend).toHaveBeenCalledExactlyOnceWith('POST', '/platform-experiments/pe-1/accept', {
      name: 'Cosine or constant', budget_accelerator_hours: 3, duration_hours: 4, baseline_minutes: 45, agents: [sweeper, {}, {}],
    })
  })

  it('accepts under the name the person gave it, and lists a researcher for each one asked for', async () => {
    show(proposed())
    const name = screen.getByLabelText('Name')
    await userEvent.clear(name)
    expect(screen.getByRole('button', { name: 'Accept and run' })).toHaveProperty('disabled', true)
    await userEvent.type(name, 'Schedules on tiny transformers')
    const researchers = screen.getByLabelText('Researchers')
    await userEvent.clear(researchers)
    await userEvent.type(researchers, '1')
    expect(screen.queryByText('Researcher 2')).toBeNull()
    await userEvent.click(screen.getByRole('button', { name: 'Accept and run' }))
    expect(apiSend).toHaveBeenCalledExactlyOnceWith('POST', '/platform-experiments/pe-1/accept', {
      name: 'Schedules on tiny transformers', budget_accelerator_hours: 6, duration_hours: 4, baseline_minutes: 30, agents: [sweeper],
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
    expect(screen.getByLabelText('Name')).toMatchObject({ value: 'Cosine or constant', disabled: true })
    await waitFor(() => expect(screen.getByRole('status').textContent).toBe('Updating the plan with your change.'))
    expect(phase()).toBe('Planning')
    expect(screen.queryByRole('button', { name: 'Accept and run' })).toBeNull()
  })

  it('shows a reader of a shared draft the proposal and no control that writes', async () => {
    render(<SWRConfig value={{ provider: () => new Map() }}><TenantContext.Provider value={tenant}><DraftView draft={proposed()} readOnly /></TenantContext.Provider></SWRConfig>)
    expect(screen.getByLabelText('Name')).toMatchObject({ value: 'Cosine or constant', disabled: true })
    expect(screen.getByLabelText('Budget (accelerator-hours)')).toHaveProperty('disabled', true)
    for (const name of ['Accept and run', 'Revise', 'Abandon this draft']) expect(screen.queryByRole('button', { name })).toBeNull()
  })

  it('is abandoned by closing it', async () => {
    show(proposed())
    await userEvent.click(screen.getByRole('button', { name: 'Abandon this draft' }))
    expect(apiSend).toHaveBeenCalledExactlyOnceWith('POST', '/platform-experiments/pe-1/close')
  })
})

// A person who had nothing to change still had to find the button at the foot of the proposal
// before anything ran.
describe('a proposal that arrives while the person watches it being planned', () => {
  const proposedAt = '2026-10-05T12:10:00Z'
  const proposed = (at = proposedAt) => draft('proposed', { proposal, proposed_at: at })
  const accepted = ['POST', '/platform-experiments/pe-1/accept', { name: 'Cosine or constant', budget_accelerator_hours: 6, duration_hours: 4, baseline_minutes: 30, agents: [sweeper, {}] }]
  const countdown = () => screen.queryByRole('timer')?.textContent
  // A second at a time: each second of the countdown is set from the render of the one before.
  const pass = async (seconds: number) => {
    for (let second = 0; second < seconds; second++) await act(() => vi.advanceTimersByTimeAsync(1_000))
  }
  const visibility = (state: 'visible' | 'hidden') => {
    vi.spyOn(document, 'visibilityState', 'get').mockReturnValue(state)
    fireEvent(document, new Event('visibilitychange'))
  }

  const page = (d: Draft, readOnly = false) => <SWRConfig value={{ provider: () => new Map() }}><TenantContext.Provider value={tenant}><DraftView draft={d} readOnly={readOnly} /></TenantContext.Provider></SWRConfig>
  // The page as it is while the planner works, then as the next read of the draft makes it.
  function arrive(to = proposed(), from = draft('planning'), readOnly = false) {
    const view = render(page(from, readOnly))
    expect(countdown()).toBeUndefined()
    view.rerender(page(to, readOnly))
    return view
  }

  beforeEach(() => { vi.useFakeTimers() })
  afterEach(() => { vi.useRealTimers() })

  it('counts down from 10 s where the person can see it and stop it', async () => {
    arrive()
    expect(countdown()).toBe('Starting in 10 s')
    expect(screen.getByRole('button', { name: 'Wait, let me edit' })).toBeTruthy()
    await pass(3)
    expect(countdown()).toBe('Starting in 7 s')
    expect(apiSend).not.toHaveBeenCalled()
  })

  it('accepts at zero with the values shown, once', async () => {
    arrive()
    await pass(10)
    expect(apiSend).toHaveBeenCalledExactlyOnceWith(...accepted)
    expect(countdown()).toBeUndefined()
    await pass(30)
    expect(apiSend).toHaveBeenCalledTimes(1)
  })

  it.each([
    ['the person asks it to wait', () => fireEvent.click(screen.getByRole('button', { name: 'Wait, let me edit' }))],
    ['a field takes the focus', () => act(() => screen.getByLabelText('Name').focus())],
    ['a key is pressed in the form', () => fireEvent.keyDown(screen.getByLabelText('Budget (accelerator-hours)'), { key: '7' })],
    ['the pointer moves over the proposal', () => fireEvent.pointerMove(screen.getByText('One seed only.'))],
    ['the person starts to ask for a change', () => fireEvent.pointerDown(screen.getByLabelText('What should be different?'))],
  ])('stops for good when %s, and accepts nothing by itself', async (_what, react) => {
    arrive()
    await pass(4)
    react()
    expect(countdown()).toBeUndefined()
    await pass(30)
    expect(apiSend).not.toHaveBeenCalled()
    expect(screen.getByRole('button', { name: 'Accept and run' })).toHaveProperty('disabled', false)
  })

  it('stops for good when the tab is hidden, and does not start again when it is shown', async () => {
    arrive()
    await pass(4)
    act(() => visibility('hidden'))
    expect(countdown()).toBeUndefined()
    act(() => visibility('visible'))
    await pass(30)
    expect(countdown()).toBeUndefined()
    expect(apiSend).not.toHaveBeenCalled()
  })

  it('does not start on a page that is hidden when the proposal arrives', async () => {
    vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('hidden')
    arrive()
    expect(countdown()).toBeUndefined()
    await pass(30)
    expect(apiSend).not.toHaveBeenCalled()
  })

  // A proposal opened days later, in another browser or after a reload was accepted and spent
  // with nobody having watched it come.
  it('never counts for a proposal that is already there when the page loads, however often it is read again', async () => {
    const loaded = show(proposed())
    expect(countdown()).toBeUndefined()
    loaded.rerender(page(proposed()))
    await pass(30)
    expect(countdown()).toBeUndefined()
    expect(apiSend).not.toHaveBeenCalled()
    expect(screen.getByRole('button', { name: 'Accept and run' })).toHaveProperty('disabled', false)
  })

  it('counts again for the proposal that follows a revision the page watched, whichever came before', async () => {
    const loaded = show(proposed())
    loaded.rerender(page(draft('revising', { proposal })))
    expect(countdown()).toBeUndefined()
    loaded.rerender(page(proposed('2026-10-05T12:40:00Z')))
    expect(countdown()).toBe('Starting in 10 s')
    fireEvent.click(screen.getByRole('button', { name: 'Wait, let me edit' }))
    loaded.rerender(page(proposed('2026-10-05T12:40:00Z')))
    await pass(30)
    expect(apiSend).not.toHaveBeenCalled()
  })

  it('shows why the API refused at zero, and does not count again', async () => {
    vi.mocked(apiSend).mockRejectedValue(new Error('accelerator_unreachable: no cluster has rtx-4090 now'))
    arrive()
    await pass(10)
    expect(screen.getByRole('alert').textContent).toMatch(/no cluster has rtx-4090 now/)
    expect(countdown()).toBeUndefined()
    await pass(30)
    expect(apiSend).toHaveBeenCalledTimes(1)
  })

  it('does not count for a reader, nor while the planner revises', async () => {
    arrive(proposed(), draft('planning'), true)
    expect(countdown()).toBeUndefined()
    cleanup()
    arrive(draft('revising', { proposal }))
    expect(countdown()).toBeUndefined()
    await pass(30)
    expect(apiSend).not.toHaveBeenCalled()
  })
})

// A goal the planner could not plan left a draft with nothing on its page until it closed.
describe('a draft the planner declined', () => {
  const why = '"Make the model better" names nothing to measure: say which model, on what data, and what better means.'
  const declined = () => draft('declined', { declined_reason: why, declined_at: '2026-10-05T12:10:00Z', planning_ends_at: undefined, proposal_expires_at: '2026-10-12T12:10:00Z' })

  it('shows the reason, nothing to accept, and the ways to answer it or drop it', async () => {
    show(declined())
    expect(screen.getByText('What the planner needs')).toBeTruthy()
    expect(screen.getByText(why)).toBeTruthy()
    expect(screen.getByText(/Draft, waiting for you/)).toBeTruthy()
    expect(screen.getByText(new RegExp(`^Answer below and it tries again\\. Otherwise the draft closes on ${formatDate('2026-10-12T12:10:00Z')}\\.$`))).toBeTruthy()
    expect(phase()).toBe('Proposal')
    expect(screen.getByRole('status').textContent).toBe('The planner needs more from you. Its reason is below.')
    expect(screen.queryByRole('button', { name: 'Accept and run' })).toBeNull()
    expect(screen.getByRole('button', { name: 'Abandon this draft' })).toBeTruthy()
    await userEvent.type(screen.getByLabelText('What was missing?'), 'The small transformer, by validation loss.')
    await userEvent.click(screen.getByRole('button', { name: 'Revise' }))
    expect(apiSend).toHaveBeenCalledExactlyOnceWith('POST', '/platform-experiments/pe-1/revise', { message: 'The small transformer, by validation loss.' })
  })
})
