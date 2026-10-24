import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { fetchPlatformExperimentAgents } from '@/lib/api/agents'
import { PhaseStrip } from '@/components/phase-strip'
import { formatDate } from '@/lib/format'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import type { StoredProposal } from '@/lib/planning'
import type { JoinedAgent, Me, PlatformExperiment, PlatformExperimentStatus, StatusReason } from '@/types/api'

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
vi.mock('@/components/platform-experiment-form', () => ({
  PlatformExperimentDialog: ({ initial }: { initial: PlatformExperiment }) => <div role="dialog">Edit experiment {initial.name}, budget {initial.budget_accelerator_hours}</div>,
}))

const at = '2026-10-05T12:30:00Z'

function experiment(status: `${PlatformExperimentStatus}`, reason: string, more: Partial<PlatformExperiment> = {}, said: Partial<StatusReason> = {}): PlatformExperiment {
  return {
    id: 'pe-1', project_id: 'prj_1', name: 'Cosine or constant', status,
    editable: [], actions: status === 'closed' ? ['remove'] : ['remove', status === 'draft' ? 'abandon' : 'close'],
    status_reason: { reason, message: `the API's message for ${reason}`, summary: `the API's summary for ${reason}`, ...(status === 'closed' ? {} : { waits_for: 'platform' as const }), ...said }, ...more,
  } as PlatformExperiment
}

function agent(role: JoinedAgent['role'], more: Partial<JoinedAgent> = {}): JoinedAgent {
  return {
    platform_experiment_id: 'pe-1', agent_id: `ag-${role}`, name: role === 'baseline' ? 'Baseline 1' : 'Planner 1', role, in_agentsandbox: true, agent_type: role === 'planner' ? 'planner' : 'generalist', model: '', hyperparameters: null,
    actor: { kind: 'coordinator', id: 'pe-1', name: 'coordinator' }, at_work: false, ended: '', signed_up_at: at, instance: { state: 'running', restarts: 0, stopped: false }, actions: [], ...more,
  }
}

const me: Me = {
  kind: 'user', user_id: 'u1', name: 'Ada',
  orgs: [{ id: 'org_a', name: 'Lab', role: 'owner', created_at: at, actions: ['edit', 'create_api_key'] }],
  projects: [{ id: 'prj_1', org_id: 'org_a', name: 'default', created_at: at }],
}
const resolved = resolveTenant(me, 'prj_1')
if (resolved.state !== 'selected') throw new Error('no tenant')
const tenant = resolved.tenant

function show(pe: PlatformExperiment) {
  return render(<SWRConfig value={{ provider: () => new Map() }}><TenantContext.Provider value={tenant}><PhaseStrip pe={pe} jobs={[]} /></TenantContext.Provider></SWRConfig>)
}

const phases = () => within(screen.getByRole('list', { name: 'Phase' })).getAllByRole('listitem')
const current = () => phases().filter(li => li.getAttribute('aria-current') === 'step').map(li => li.textContent)
const sentence = () => screen.getByRole('status').textContent

beforeEach(() => {
  vi.mocked(fetchPlatformExperimentAgents).mockResolvedValue([])
})

describe('the phase strip', () => {
  it.each([
    ['draft', 'planning', {}, 'Planning'],
    ['draft', 'revising', {}, 'Planning'],
    ['draft', 'proposed', { proposed_at: at }, 'Proposal'],
    ['draft', 'declined', {}, 'Proposal'],
    ['open', 'baseline_measuring', {}, 'Preparing baseline'],
    ['open', 'baseline_required', {}, 'Preparing baseline'],
    ['open', 'budget_below_baseline', {}, 'Preparing baseline'],
    ['open', 'starts_at_ahead', {}, 'Preparing baseline'],
    ['open', 'starting', {}, 'Preparing baseline'],
    ['running', 'agents_starting', {}, 'Running'],
    ['running', 'running', {}, 'Running'],
    ['closed', 'end_time_reached', {}, 'Closed'],
    ['closed', 'agents_not_started', {}, 'Closed'],
    ['closed', 'planner_stopped', {}, 'Closed'],
  ] as const)('stands at one phase, from the status, its reason and whether a proposal stands: %s, %s', (status, reason, more, phase) => {
    show(experiment(status, reason, more))
    expect(current()).toEqual([phase])
  })

  it('says why it closed in the API\'s summary, after when it closed', () => {
    const said = 'It was closed early by Ada, who said: the baseline measured the old split'
    show(experiment('closed', 'closed_by_request', { closed_at: at }, { summary: said }))
    expect(sentence()).toBe(`Closed ${formatDate(at)}. ${said}`)
  })

  it('says the baseline wait in the API\'s summary, after what it prepares', async () => {
    vi.mocked(fetchPlatformExperimentAgents).mockResolvedValue([agent('baseline')])
    show(experiment('open', 'baseline_measuring', {}, { summary: 'Baseline 1 is measuring the baseline. The run starts by itself when it finishes.' }))
    expect(sentence()).toBe('Preparing the baseline. Baseline 1 is measuring the baseline. The run starts by itself when it finishes.')
    expect(await screen.findByRole('group', { name: 'Baseline agent' })).toBeTruthy()
  })

  it('refuses an experiment that came without the reason every read of one carries', () => {
    vi.spyOn(console, 'error').mockImplementation(() => {})
    expect(() => show({ ...experiment('draft', 'planning'), status_reason: undefined })).toThrow(/came without a status_reason/)
  })
})

describe('whose move it is', () => {
  const waits = (reason: string, who: StatusReason['waits_for'], more: Partial<PlatformExperiment> = {}) =>
    experiment('open', reason, { baseline_job_id: 'job-base', ...more }, { waits_for: who, summary: `It waits: ${reason}.` })
  const strip = () => within(screen.getByRole('region', { name: 'Where this experiment stands' }))

  it('says so prominently when it is the owner\'s, in the API\'s words, with the way to add an executor', () => {
    show(waits('no_executors', 'owner'))
    expect(strip().getByText('Your move')).toBeTruthy()
    expect(sentence()).toBe('It waits: no_executors.')
    expect(strip().getByRole('link', { name: 'Add an executor' }).getAttribute('href')).toBe('#agents')
    expect(strip().queryByText(/Nothing is needed from you/)).toBeNull()
  })

  it.each([
    ['budget_spent', 'Edit budget'],
    ['budget_below_baseline', 'Edit budget'],
  ])('opens the experiment\'s form when %s is what it waits for', async (reason, label) => {
    show(waits(reason, 'owner', { budget_accelerator_hours: 2 }))
    expect(screen.queryByRole('dialog')).toBeNull()
    await userEvent.click(strip().getByRole('button', { name: label }))
    expect(screen.getByRole('dialog').textContent).toBe('Edit experiment Cosine or constant, budget 2')
  })

  it('refuses a reason that waits for the owner and has no entry in the page\'s table, and an open experiment that names nobody', () => {
    vi.spyOn(console, 'error').mockImplementation(() => {})
    expect(() => show(waits('a_reason_of_tomorrow', 'owner'))).toThrow(/waits for the owner and the page has no entry/)
    expect(() => show(waits('starting', undefined))).toThrow(/came without waits_for/)
  })

  it.each([
    ['coordinator', 'Nothing is needed from you: its coordinator acts next.'],
    ['platform', 'Nothing is needed from you: the platform acts next.'],
  ] as const)('says plainly that nothing is needed when the move is the %s\'s', (who, plainly) => {
    show(waits('baseline_required', who))
    expect(strip().getByText(plainly)).toBeTruthy()
    expect(strip().queryByText('Your move')).toBeNull()
  })

  it('tells a reader of a shared experiment neither, and a closed one needs nobody', () => {
    const shared = render(<SWRConfig value={{ provider: () => new Map() }}><PhaseStrip pe={{ ...waits('no_executors', 'owner'), actions: [] }} /></SWRConfig>)
    expect(sentence()).toBe('It waits: no_executors.')
    expect(strip().queryByText('Your move')).toBeNull()
    expect(strip().queryByRole('link')).toBeNull()
    shared.unmount()
    show(experiment('closed', 'end_time_reached'))
    expect(strip().queryByText(/Your move|Nothing is needed/)).toBeNull()
  })
})

describe('the baseline agent in the phase strip', () => {
  it('is not shown once the run started, and is the one still measuring when another finished', async () => {
    vi.mocked(fetchPlatformExperimentAgents).mockResolvedValue([
      agent('baseline', { agent_id: 'ag-first', name: 'Baseline 1', finished_at: at, finished_reason: 'the harness does not build' }),
      agent('baseline', { agent_id: 'ag-second', name: 'Baseline 2' }),
    ])
    const first = show(experiment('open', 'baseline_measuring'))
    expect((await screen.findByRole('group', { name: 'Baseline agent' })).textContent).toContain('Baseline 2')
    first.unmount()
    show(experiment('running', 'running', { ends_at: at }))
    expect(screen.queryByRole('group', { name: 'Baseline agent' })).toBeNull()
  })
})

// Once a proposal was accepted the page had no way back to it: what was proposed, the planner's
// questions and the answers given, and who accepted it were gone from sight.
describe('the proposal of an accepted experiment', () => {
  const proposal: StoredProposal = {
    experiment: {
      name: 'Schedules on tiny transformers',
      description: 'Train the small transformer in train.py and report val_loss.',
      metrics: [{ key: 'val_loss', direction: 'minimize' }],
    },
    budget_accelerator_hours: 6,
    duration_hours: 4,
    baseline_minutes: 30,
    goal_version: 1,
    agents: [{}, {}],
    cpu: '4',
    memory: '16Gi',
    storage: '20Gi',
    accelerator_type: 'rtx-4090',
    accelerator_kind: 'accelerator',
    metric_reason: 'Validation loss on held-out text is the goal itself.',
    measurement: 'bench.py scores a split the training never reads.',
    sizing_reason: 'An estimate, nothing has run.',
    questions: [
      { question: 'How big a win counts?', answer: '20% lower loss.' },
      { question: 'Which batch sizes are held equal?', answer: '32 only.' },
    ],
  }
  const accepted = { proposal, proposed_at: '2026-10-05T12:10:00Z', accepted_at: '2026-10-05T12:15:00Z' }
  const step = () => within(screen.getByRole('list', { name: 'Phase' })).queryByRole('button', { name: 'Proposal' })

  it('opens from the Proposal step as a record, with who accepted it and the answers given, and nothing to act on', async () => {
    show(experiment('running', 'running', { ...accepted, accepted_by: { kind: 'user', id: 'u-1', name: 'Ada' }, accepted_answers: ['20% lower loss.', '32 and 64.'] }))
    expect(screen.queryByText('How big a win counts?')).toBeNull()
    await userEvent.click(step()!)
    expect(step()!.getAttribute('aria-pressed')).toBe('true')
    expect(screen.getByRole('note').textContent).toBe(`Accepted by Ada on ${formatDate('2026-10-05T12:15:00Z')}.`)
    expect(screen.getByText('How big a win counts?')).toBeTruthy()
    expect(screen.getByText('32 and 64.')).toBeTruthy()
    expect(screen.getByText('The planner had planned on: 32 only.')).toBeTruthy()
    expect(screen.getByText('Schedules on tiny transformers')).toBeTruthy()
    expect(screen.getByText('val_loss, lower is better')).toBeTruthy()
    expect(await screen.findAllByText('Executor')).toHaveLength(2)
    expect(screen.queryAllByRole('textbox')).toHaveLength(0)
    expect(screen.queryAllByRole('spinbutton')).toHaveLength(0)
    for (const name of ['Accept and run', 'Accept now', 'Edit', 'Reject']) expect(screen.queryByRole('button', { name })).toBeNull()
    await userEvent.click(step()!)
    expect(screen.queryByText('How big a win counts?')).toBeNull()
  })

  it('says when the platform accepted it by itself', async () => {
    show(experiment('closed', 'end_time_reached', { ...accepted, proposal: { ...proposal, questions: undefined } }))
    await userEvent.click(step()!)
    expect(screen.getByRole('note').textContent).toBe(`Accepted automatically on ${formatDate('2026-10-05T12:15:00Z')}.`)
  })

  it('is no step to open on a draft, whose proposal is on its page, nor on one created whole', () => {
    const draft = show(experiment('draft', 'proposed', { proposal, proposed_at: accepted.proposed_at }))
    expect(step()).toBeNull()
    draft.unmount()
    show(experiment('open', 'baseline_measuring'))
    expect(step()).toBeNull()
  })
})
