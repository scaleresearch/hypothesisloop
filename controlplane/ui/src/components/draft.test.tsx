import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SWRConfig } from 'swr'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { fetchPlatformExperimentAgents } from '@/lib/api/agents'
import { apiSend } from '@/lib/api/request'
import { DraftView } from '@/components/draft'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import { formatDate } from '@/lib/format'
import type { StoredProposal } from '@/lib/planning'
import type { JoinedAgent, Me, PlatformExperiment } from '@/types/api'

vi.mock('next/navigation', () => ({
  useRouter: () => ({ push: vi.fn() }),
  usePathname: () => '/experiments/pe-1',
  useSearchParams: () => new URLSearchParams(),
}))
vi.mock('@/lib/api/agents', () => ({ fetchPlatformExperimentAgents: vi.fn() }))
vi.mock('@/lib/api/request', async importActual => ({
  ...(await importActual<typeof import('@/lib/api/request')>()),
  apiSend: vi.fn(),
}))

const sweeper = { agent_type: 'hyperparameter-search', model: 'claude-opus-4-5', hyperparameters: { direction: 'Find the schedule: warmup length first, then the decay.' } }
const proposal: StoredProposal = {
  experiment: {
    name: 'Cosine or constant',
    description: 'Train the small transformer in train.py and report val_loss.',
    metrics: [
      { key: 'val_loss', direction: 'minimize', target_vs_baseline: -0.2 },
      { key: 'exact_match', direction: 'maximize', role: 'constraint', bound: 1 },
    ],
    report_interval_seconds: 30,
  },
  budget_accelerator_hours: 6,
  duration_hours: 4,
  baseline_minutes: 30,
  goal_version: 1,
  agents: [sweeper, {}],
  cpu: '4',
  memory: '16Gi',
  storage: '20Gi',
  accelerator_type: 'rtx-4090',
  accelerator_kind: 'accelerator',
  image: 'pytorch',
  metric_reason: 'Validation loss on held-out text is the goal itself.',
  measurement: 'bench.py scores a split the training never reads.',
  sizing_reason: 'An estimate, nothing has run: a trial is assumed to take 6 minutes, so 30 trials each for 2 researchers is 6.',
  risks: ['One seed only.'],
}

const me: Me = {
  kind: 'user',
  user_id: 'u1',
  orgs: [{ id: 'org_a', name: 'Ada', role: 'owner', created_at: '2026-01-01T00:00:00Z', actions: ['edit', 'create_api_key'] }],
  projects: [{ id: 'prj_1', org_id: 'org_a', name: 'default', created_at: '2026-01-01T00:00:00Z' }],
}
const resolved = resolveTenant(me, 'prj_1')
if (resolved.state !== 'selected') throw new Error('no tenant')
const tenant = resolved.tenant

function draft(reason: 'planning' | 'revising' | 'accepting' | 'proposed' | 'declined', more: Partial<PlatformExperiment> = {}): PlatformExperiment {
  return {
    id: 'pe-1', project_id: 'prj_1', name: 'Does a cosine schedule beat…', status: 'draft',
    status_reason: { reason, message: '', summary: `the API's summary for ${reason}`, waits_for: reason === 'proposed' || reason === 'declined' ? 'owner' : 'platform' },
    goal: 'Does a cosine schedule beat a constant one?', goal_version: 1, editable: ['goal'],
    actions: ['remove', 'abandon', ...(reason === 'proposed' || reason === 'accepting' ? ['accept' as const] : [])],
    planning_ends_at: '2026-10-05T12:30:00Z', ...more,
  } as PlatformExperiment
}

// As the API answers a draft to one who writes nothing of it.
const reader = (d: PlatformExperiment): PlatformExperiment => ({ ...d, editable: [], actions: [] })

function show(d: PlatformExperiment) {
  return render(<SWRConfig value={{ provider: () => new Map() }}><TenantContext.Provider value={tenant}><DraftView draft={d} /></TenantContext.Provider></SWRConfig>)
}

beforeEach(() => {
  vi.mocked(apiSend).mockReset().mockResolvedValue({})
  vi.mocked(fetchPlatformExperimentAgents).mockResolvedValue([
    { platform_experiment_id: 'pe-1', agent_id: 'ag-p', name: 'Planner', role: 'planner', in_agentsandbox: true, agent_type: 'planner', signed_up_at: '', instance: { state: 'starting', restarts: 0, stopped: false } } as JoinedAgent,
  ])
})

const phase = () => within(screen.getByRole('list', { name: 'Phase' })).getAllByRole('listitem').filter(li => li.getAttribute('aria-current') === 'step').map(li => li.textContent).join()
const planningStep = () => within(screen.getByRole('list', { name: 'Planning' })).queryAllByRole('listitem').find(li => li.getAttribute('aria-current') === 'step')?.textContent

// The page said one sentence for as long as planning took, and the same one whether the
// planner's machine was still being rented or the planner was reading the repository.
describe('a draft being planned', () => {
  it('says the phase and where the planner\'s machine stands, and offers nothing to accept', async () => {
    show(draft('planning'))
    expect(screen.getByText('Does a cosine schedule beat a constant one?')).toBeTruthy()
    expect(screen.queryByText(/Nothing is needed from you/)).toBeNull()
    expect(phase()).toBe('Planning')
    await waitFor(() => expect(screen.getByRole('status').textContent).toBe('Starting the planner…'))
    expect(screen.getByText(/so far\./)).toBeTruthy()
    expect(planningStep()).toBe('Starting the planner')
    expect(screen.queryByRole('button', { name: 'Accept and run' })).toBeNull()
    expect(screen.getByRole('button', { name: 'Edit what you asked' })).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Abandon this draft' })).toBeTruthy()
  })

  describe('reads the roster', () => {
    beforeEach(() => { vi.useFakeTimers() })
    afterEach(() => { vi.useRealTimers() })
    const reads = () => vi.mocked(fetchPlatformExperimentAgents).mock.calls.length

    it('once while the planner works, and on no timer: the change stream reads it again', async () => {
      vi.mocked(fetchPlatformExperimentAgents).mockClear()
      show(draft('planning'))
      await vi.advanceTimersByTimeAsync(100)
      expect(reads()).toBe(1)
      await vi.advanceTimersByTimeAsync(60_000)
      expect(reads()).toBe(1)
    })
  })
})

describe('a proposed draft', () => {
  const proposed = () => draft('proposed', { proposal, proposed_at: '2026-10-05T12:10:00Z' })

  // The page once started the experiment right after accepting it. The platform starts it,
  // once its baseline is declared: accepting is the one call.
  it('accepts at the cost the person states, keeps the proposed agent types, and starts nothing', async () => {
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

  it('changes what the person asked', async () => {
    show(proposed())
    await userEvent.click(screen.getByRole('button', { name: 'Edit what you asked' }))
    expect(screen.getByText('Saving starts planning again from this text.')).toBeTruthy()
    const save = screen.getByRole('button', { name: 'Save' })
    expect(save).toHaveProperty('disabled', true)
    await userEvent.type(screen.getByLabelText('Goal'), ' It must fit one RTX 4090.')
    await userEvent.click(save)
    expect(apiSend).toHaveBeenCalledExactlyOnceWith('PATCH', '/platform-experiments/pe-1', { goal: 'Does a cosine schedule beat a constant one? It must fit one RTX 4090.' })
  })

  it('keeps the proposal readable while it is planned again from the changed goal, and takes no acceptance', async () => {
    show(draft('revising', { proposal, goal: 'Does a cosine schedule beat a constant one? It must fit one RTX 4090.', goal_version: 2 }))
    expect(screen.getByText('Does a cosine schedule beat a constant one? It must fit one RTX 4090.')).toBeTruthy()
    expect(screen.getByLabelText('Name')).toMatchObject({ value: 'Cosine or constant', disabled: true })
    await waitFor(() => expect(screen.getByRole('status').textContent).toMatch(/^Updating the plan with your change\. /))
    expect(screen.queryByText(/so far\./)).toBeNull()
    expect(phase()).toBe('Planning')
    expect(screen.queryByRole('button', { name: 'Accept and run' })).toBeNull()
  })

  it('shows a reader of a shared draft the proposal and no control that writes', async () => {
    render(<SWRConfig value={{ provider: () => new Map() }}><TenantContext.Provider value={tenant}><DraftView draft={reader(proposed())} /></TenantContext.Provider></SWRConfig>)
    expect(screen.getByLabelText('Name')).toMatchObject({ value: 'Cosine or constant', disabled: true })
    expect(screen.getByLabelText('Budget (accelerator-hours)')).toHaveProperty('disabled', true)
    expect(screen.getByText('val_loss, lower is better, target -20% against the baseline')).toBeTruthy()
    for (const name of ['Accept and run', 'Edit what you asked', 'Abandon this draft']) expect(screen.queryByRole('button', { name })).toBeNull()
  })

  it('is abandoned by closing it', async () => {
    show(proposed())
    await userEvent.click(screen.getByRole('button', { name: 'Abandon this draft' }))
    expect(apiSend).not.toHaveBeenCalled()
    await userEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Abandon draft' }))
    expect(apiSend).toHaveBeenCalledExactlyOnceWith('POST', '/platform-experiments/pe-1/close')
  })
})

// A person who had nothing to change still had to find the button at the foot of the proposal
// before anything ran, and one away from the page held the run up for as long as they were away.
// The platform accepts a proposal by itself at accepts_at, and the page counts down to it.
describe('a proposal the platform accepts by itself unless the person acts first', () => {
  const accepted = ['POST', '/platform-experiments/pe-1/accept', { name: 'Cosine or constant', budget_accelerator_hours: 6, duration_hours: 4, baseline_minutes: 30, agents: [sweeper, {}] }]
  const accepting = (seconds = 30) => draft('accepting', { proposal, proposed_at: '2026-10-05T12:10:00Z', accepts_at: new Date(Date.now() + seconds * 1_000).toISOString() })
  const countdown = () => screen.queryByRole('timer')?.textContent
  const pass = async (seconds: number) => {
    for (let second = 0; second < seconds; second++) await act(() => vi.advanceTimersByTimeAsync(1_000))
  }

  beforeEach(() => { vi.useFakeTimers() })
  afterEach(() => { vi.useRealTimers() })

  it('counts down to accepts_at where the person can see it, with the proposal only read', async () => {
    show(accepting(30))
    expect(countdown()).toBe('Starting in 30 s')
    expect(screen.getByText(/Draft, starting in a moment/)).toBeTruthy()
    expect(screen.getByLabelText('Name')).toHaveProperty('disabled', true)
    expect(screen.queryByRole('button', { name: 'Accept and run' })).toBeNull()
    await pass(3)
    expect(countdown()).toBe('Starting in 27 s')
    await pass(30)
    expect(countdown()).toBe('Starting now')
    expect(apiSend).not.toHaveBeenCalled()
  })

  it('accepts now with the values shown', async () => {
    show(accepting())
    fireEvent.click(screen.getByRole('button', { name: 'Accept now' }))
    await pass(1)
    expect(apiSend).toHaveBeenCalledExactlyOnceWith(...accepted)
  })

  it.each([
    ['Edit', '/platform-experiments/pe-1/hold'],
    ['Reject', '/platform-experiments/pe-1/close'],
  ])('%s stops the countdown on the platform', async (button, path) => {
    show(accepting())
    fireEvent.click(screen.getByRole('button', { name: button }))
    await pass(1)
    expect(apiSend).toHaveBeenCalledExactlyOnceWith('POST', path)
  })

  it('says why the platform refused to hold it', async () => {
    vi.mocked(apiSend).mockRejectedValue(new Error('draft pe-1 has no proposal standing to hold'))
    show(accepting())
    fireEvent.click(screen.getByRole('button', { name: 'Edit' }))
    await pass(1)
    expect(screen.getByRole('alert').textContent).toMatch(/no proposal standing/)
  })

  it('shows a reader the proposal and no countdown', () => {
    render(<SWRConfig value={{ provider: () => new Map() }}><TenantContext.Provider value={tenant}><DraftView draft={reader(accepting())} /></TenantContext.Provider></SWRConfig>)
    expect(countdown()).toBeUndefined()
    for (const name of ['Accept now', 'Edit', 'Reject']) expect(screen.queryByRole('button', { name })).toBeNull()
  })
})

// The planner wrote what the goal left open ("no margin was stated, so I set 1.5x") and the
// platform accepted it 30 s later, so the agents ran on its guesses. A proposal with questions
// waits for the person, who confirms or changes each answer the planner planned on.
describe('a proposal with questions', () => {
  const asking = () => draft('proposed', {
    proposed_at: '2026-10-05T12:10:00Z',
    proposal: { ...proposal, questions: [
      { question: 'How big a win counts?', answer: 'A 20% lower val_loss.' },
      { question: 'Which batch size is held equal?', answer: '32.' },
    ] },
  })

  it('shows each question with the planner\'s answer and accepts with the answers as edited', async () => {
    show(asking())
    expect(screen.getByLabelText('How big a win counts?')).toHaveProperty('value', 'A 20% lower val_loss.')
    const batch = screen.getByLabelText('Which batch size is held equal?')
    await userEvent.clear(batch)
    await userEvent.type(batch, '64, the size the control ships with. ')
    await userEvent.click(screen.getByRole('button', { name: 'Accept and run' }))
    expect(apiSend).toHaveBeenCalledExactlyOnceWith('POST', '/platform-experiments/pe-1/accept', {
      name: 'Cosine or constant', budget_accelerator_hours: 6, duration_hours: 4, baseline_minutes: 30, agents: [sweeper, {}],
      answers: ['A 20% lower val_loss.', '64, the size the control ships with.'],
    })
  })

  it('shows a reader the questions and answers with nothing to edit', () => {
    render(<SWRConfig value={{ provider: () => new Map() }}><TenantContext.Provider value={tenant}><DraftView draft={reader(asking())} /></TenantContext.Provider></SWRConfig>)
    expect(screen.getByLabelText('How big a win counts?')).toMatchObject({ value: 'A 20% lower val_loss.', disabled: true })
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
    expect(screen.getByText(new RegExp(`^Edit what you asked and the planner tries again\\. Otherwise the draft closes on ${formatDate('2026-10-12T12:10:00Z')}\\.$`))).toBeTruthy()
    expect(phase()).toBe('Proposal')
    expect(screen.getByRole('status').textContent).toBe('the API\'s summary for declined')
    expect(screen.queryByRole('button', { name: 'Accept and run' })).toBeNull()
    expect(screen.getByRole('button', { name: 'Abandon this draft' })).toBeTruthy()
    await userEvent.click(screen.getByRole('button', { name: 'Edit what you asked' }))
    await userEvent.clear(screen.getByLabelText('Goal'))
    await userEvent.type(screen.getByLabelText('Goal'), 'The small transformer, by validation loss.')
    await userEvent.click(screen.getByRole('button', { name: 'Save' }))
    expect(apiSend).toHaveBeenCalledExactlyOnceWith('PATCH', '/platform-experiments/pe-1', { goal: 'The small transformer, by validation loss.' })
  })
})

describe('what the person asked', () => {
  it('keeps the lines of the brief', () => {
    show(draft('proposed', { goal: 'Find the best schedule.\nSpell out: val_loss only.' }))
    const goal = screen.getByText(/Find the best schedule\./)
    expect(goal.className).toContain('whitespace-pre-line')
    expect(goal.textContent).toBe('Find the best schedule.\nSpell out: val_loss only.')
  })
})
