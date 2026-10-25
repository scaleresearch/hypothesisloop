import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SWRConfig } from 'swr'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { fetchPlatformExperimentAgents } from '@/lib/api/agents'
import { apiSend } from '@/lib/api/request'
import { AbandonDraft, DraftSections } from '@/components/draft'
import { Plan } from '@/components/plan'
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
vi.mock('@/lib/api/agent-types', async () => {
  const { CATALOG, ROLES } = await import('@/test-agent-types')
  return { fetchAgentTypes: vi.fn(async () => CATALOG), fetchAgentRoles: vi.fn(async () => ROLES) }
})
vi.mock('@/lib/api/models', async () => ({ fetchModels: vi.fn(async () => (await import('@/test-models')).MODELS) }))
vi.mock('@/components/platform-experiment-form', () => ({
  PlatformExperimentDialog: ({ initial }: { initial: PlatformExperiment }) => <div role="dialog">Edit experiment {initial.name}</div>,
}))
vi.mock('@/lib/api/request', async importActual => ({
  ...(await importActual<typeof import('@/lib/api/request')>()),
  apiSend: vi.fn(),
}))

const sweeper = { agent_type: 'hyperparameter-search', model: 'anthropic/claude-opus-5-5', hyperparameters: { direction: 'Find the schedule: warmup length first, then the decay.' } }
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
  metric_reason: 'Validation loss on held-out text is the goal itself.',
  measurement: 'bench.py scores a split the training never reads.',
  sizing_reason: 'An estimate, nothing has run: a trial is assumed to take 6 minutes, so 30 trials each for 2 executors is 6.',
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

function draft(reason: 'planning' | 'revising' | 'declined', more: Partial<PlatformExperiment> = {}): PlatformExperiment {
  return {
    id: 'pe-1', project_id: 'prj_1', name: 'Does a cosine schedule beat…', status: 'draft',
    status_reason: { reason, message: '', summary: `the API's summary for ${reason}`, waits_for: reason === 'declined' ? 'owner' : 'platform' },
    goal: 'Does a cosine schedule beat a constant one?', goal_version: 1, editable: ['goal'],
    actions: ['remove', 'abandon'],
    planning_ends_at: '2026-10-05T12:30:00Z', ...more,
  } as PlatformExperiment
}

// As the API answers a draft to one who writes nothing of it.
const reader = (d: PlatformExperiment): PlatformExperiment => ({ ...d, editable: [], actions: [] })

function show(d: PlatformExperiment) {
  return render(
    <SWRConfig value={{ provider: () => new Map() }}>
      <TenantContext.Provider value={tenant}>
        <DraftSections pe={d} onChanged={() => {}} />
        {d.actions.includes('abandon') && <AbandonDraft pe={d} onAbandoned={() => {}} />}
      </TenantContext.Provider>
    </SWRConfig>,
  )
}

beforeEach(() => {
  vi.mocked(apiSend).mockReset().mockResolvedValue({})
  vi.mocked(fetchPlatformExperimentAgents).mockResolvedValue([
    { platform_experiment_id: 'pe-1', agent_id: 'ag-p', name: 'Planner', role: 'planner', in_agentsandbox: true, agent_type: 'planner', signed_up_at: '', instance: { state: 'starting', restarts: 0, stopped: false } } as JoinedAgent,
  ])
})

const phase = () => within(screen.getByRole('list', { name: 'Phase' })).getAllByRole('listitem').filter(li => li.getAttribute('aria-current') === 'step').map(li => li.textContent).join()
const steps = () => within(screen.getByRole('list', { name: 'Phase' })).getAllByRole('listitem').map(li => li.textContent)

// The page said one sentence for as long as planning took, and the same one whether the
// planner's machine was still being rented or the planner was reading the repository.
describe('a draft being planned', () => {
  it('says the phase on the one steps line and where the planner\'s machine stands', async () => {
    show(draft('planning'))
    expect(screen.getByText('Does a cosine schedule beat a constant one?')).toBeTruthy()
    expect(steps()).toEqual(['Planning', 'Baseline', 'Running', 'Closed'])
    expect(phase()).toBe('Planning')
    await waitFor(() => expect(screen.getByRole('status').textContent).toBe('Starting the planner…'))
    expect(screen.getByText(/so far\./)).toBeTruthy()
    expect(screen.queryByRole('region', { name: 'Plan' })).toBeNull()
    expect(screen.getByRole('button', { name: 'Edit what you asked' })).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Abandon draft' })).toBeTruthy()
  })

  it('starts the line at the repository while the platform creates it', () => {
    show(draft('planning', { status_reason: { reason: 'repository_creating', message: '', summary: 'Creating the repository.', waits_for: 'platform' } }))
    expect(steps()).toEqual(['Repository', 'Planning', 'Baseline', 'Running', 'Closed'])
    expect(phase()).toBe('Repository')
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

  it('changes what the person asked', async () => {
    show(draft('planning'))
    await userEvent.click(screen.getByRole('button', { name: 'Edit what you asked' }))
    expect(screen.getByText('Saving starts planning again from this text.')).toBeTruthy()
    const save = screen.getByRole('button', { name: 'Save' })
    expect(save).toHaveProperty('disabled', true)
    await userEvent.type(screen.getByLabelText('Goal'), ' It must fit one RTX 4090.')
    await userEvent.click(save)
    expect(apiSend).toHaveBeenCalledExactlyOnceWith('PATCH', '/platform-experiments/pe-1', { goal: 'Does a cosine schedule beat a constant one? It must fit one RTX 4090.' })
  })

  it('shows the planner\'s working plan, marked Draft, with nothing to edit yet', () => {
    show(draft('planning', { proposal }))
    expect(screen.getByRole('heading', { name: /Plan/ }).textContent).toBe('PlanDraft')
    expect(screen.getByText('val_loss, lower is better, target -20% against the baseline')).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Edit' })).toBeNull()
    expect(phase()).toBe('Planning')
  })

  it('says it plans again from a changed goal', async () => {
    show(draft('revising', { goal: 'Does a cosine schedule beat a constant one? It must fit one RTX 4090.', goal_version: 2 }))
    await waitFor(() => expect(screen.getByRole('status').textContent).toMatch(/^Updating the plan with your change\. /))
    expect(phase()).toBe('Planning')
  })

  it('shows a reader of a shared draft no control that writes', () => {
    show(reader(draft('planning')))
    for (const name of ['Edit what you asked', 'Abandon draft']) expect(screen.queryByRole('button', { name })).toBeNull()
  })

  it('is abandoned by closing it', async () => {
    show(draft('planning'))
    await userEvent.click(screen.getByRole('button', { name: 'Abandon draft' }))
    expect(apiSend).not.toHaveBeenCalled()
    await userEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Abandon draft' }))
    expect(apiSend).toHaveBeenCalledExactlyOnceWith('POST', '/platform-experiments/pe-1/close')
  })
})

// A goal the planner could not plan left a draft with nothing on its page until it closed.
describe('a draft the planner declined', () => {
  const why = '"Make the model better" names nothing to measure: say which model, on what data, and what better means.'
  const declined = () => draft('declined', { declined_reason: why, declined_at: '2026-10-05T12:10:00Z', planning_ends_at: undefined, proposal_expires_at: '2026-10-12T12:10:00Z' })

  it('shows the reason and the ways to answer it or drop it', async () => {
    show(declined())
    expect(screen.getByText('What the planner needs')).toBeTruthy()
    expect(screen.getByText(why)).toBeTruthy()
    expect(screen.getByText(new RegExp(`^Edit what you asked and the planner tries again\\. Otherwise the draft closes on ${formatDate('2026-10-12T12:10:00Z')}\\.$`))).toBeTruthy()
    expect(phase()).toBe('Planning')
    expect(screen.getByRole('status').textContent).toBe('the API\'s summary for declined')
    expect(screen.getByRole('button', { name: 'Abandon draft' })).toBeTruthy()
    await userEvent.click(screen.getByRole('button', { name: 'Edit what you asked' }))
    await userEvent.clear(screen.getByLabelText('Goal'))
    await userEvent.type(screen.getByLabelText('Goal'), 'The small transformer, by validation loss.')
    await userEvent.click(screen.getByRole('button', { name: 'Save' }))
    expect(apiSend).toHaveBeenCalledExactlyOnceWith('PATCH', '/platform-experiments/pe-1', { goal: 'The small transformer, by validation loss.' })
  })
})

describe('what the person asked', () => {
  it('keeps the lines of the brief', () => {
    show(draft('planning', { goal: 'Find the best schedule.\nSpell out: val_loss only.' }))
    const goal = screen.getByText(/Find the best schedule\./)
    expect(goal.className).toContain('whitespace-pre-line')
    expect(goal.textContent).toBe('Find the best schedule.\nSpell out: val_loss only.')
  })
})

// A run opened at once on its proposal, so the plan is what the page keeps of it: summed up in
// one line, the rest folded, with the one Edit.
describe('the plan of an open experiment', () => {
  const open = (more: Partial<PlatformExperiment> = {}): PlatformExperiment => ({
    ...draft('planning'), status: 'open', name: 'Cosine or constant',
    status_reason: { reason: 'baseline_measuring', message: '', summary: 'Measuring.', waits_for: 'platform' },
    proposal: { ...proposal, questions: [{ question: 'How big a win counts?', answer: 'A 20% lower val_loss.' }] },
    proposed_at: '2026-10-05T12:10:00Z', accepted_at: '2026-10-05T12:10:00Z',
    metrics: proposal.experiment.metrics, description: proposal.experiment.description,
    budget_accelerator_hours: 6, duration_hours: 4, agents: [{ ...sweeper, count: 1 }, { agent_type: 'generalist', count: 1 }],
    editable: ['budget_accelerator_hours', 'duration_hours'], ...more,
  } as PlatformExperiment)
  const showPlan = (pe: PlatformExperiment) => render(<SWRConfig value={{ provider: () => new Map() }}><TenantContext.Provider value={tenant}><Plan pe={pe} /></TenantContext.Provider></SWRConfig>)

  it('says the metric, machine, budget, length and team in one line and the rest on request', async () => {
    showPlan(open())
    expect(screen.getByText('val_loss, lower is better, target -20% against the baseline')).toBeTruthy()
    expect(screen.getByText('2 agents')).toBeTruthy()
    expect(screen.queryByText('How big a win counts?')).toBeNull()
    await userEvent.click(screen.getByRole('button', { name: 'Show the full plan' }))
    expect(screen.getByText('How big a win counts?')).toBeTruthy()
    expect(screen.getByText('A 20% lower val_loss.')).toBeTruthy()
    expect(screen.getByText('bench.py scores a split the training never reads.')).toBeTruthy()
    expect(screen.getByText('Find the schedule: warmup length first, then the decay.')).toBeTruthy()
    expect(screen.getByText('One seed only.')).toBeTruthy()
  })

  it('opens what may change now from its one Edit, and has none for a reader', async () => {
    const first = showPlan(open())
    await userEvent.click(screen.getByRole('button', { name: 'Edit' }))
    expect(screen.getByRole('dialog').textContent).toContain('Edit experiment Cosine or constant')
    first.unmount()
    showPlan(open({ editable: [] }))
    expect(screen.queryByRole('button', { name: 'Edit' })).toBeNull()
  })
})
