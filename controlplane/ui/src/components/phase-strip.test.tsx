import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { fetchPlatformExperimentAgents } from '@/lib/api'
import { PhaseStrip } from '@/components/phase-strip'
import { formatDate } from '@/lib/format'
import type { Draft, Proposal } from '@/lib/planning'
import { PlatformExperimentStatus } from '@/types'
import type { JoinedAgent, StatusReason } from '@/types'

vi.mock('next/navigation', () => ({
  useRouter: () => ({ push: vi.fn() }),
  usePathname: () => '/experiments/pe-1',
  useSearchParams: () => new URLSearchParams(),
}))
vi.mock('@/lib/api', () => ({ fetchPlatformExperimentAgents: vi.fn() }))
vi.mock('@/components/platform-experiment-form', () => ({
  PlatformExperimentDialog: ({ initial }: { initial: Draft }) => <div role="dialog">Edit experiment {initial.name}, budget {initial.budget_accelerator_hours}</div>,
}))

const at = '2026-10-05T12:30:00Z'

function experiment(status: `${PlatformExperimentStatus}`, reason: string, more: Partial<Draft> = {}, said: Partial<StatusReason> = {}): Draft {
  return {
    id: 'pe-1', project_id: 'prj_1', name: 'Cosine or constant', status,
    status_reason: { reason, message: `the API's sentence for ${reason}`, ...(status === 'closed' ? {} : { waits_for: 'platform' as const }), ...said }, ...more,
  } as Draft
}

function agent(role: JoinedAgent['role'], more: Partial<JoinedAgent> = {}): JoinedAgent {
  return {
    platform_experiment_id: 'pe-1', agent_id: `ag-${role}`, name: role === 'baseline' ? 'Baseline 1' : 'Planner 1', role, hosted: true, flavor: role === 'planner' ? 'planner' : 'generalist',
    actor: { kind: 'coordinator', id: 'pe-1', name: 'coordinator' }, signed_up_at: at, instance: { state: 'running', restarts: 0, stopped: false }, ...more,
  }
}

function show(pe: Draft) {
  return render(<SWRConfig value={{ provider: () => new Map() }}><PhaseStrip pe={pe} jobs={[]} /></SWRConfig>)
}

const phases = () => within(screen.getByRole('list', { name: 'Phase' })).getAllByRole('listitem')
const current = () => phases().filter(li => li.getAttribute('aria-current') === 'step').map(li => li.textContent)
const sentence = () => screen.getByRole('status').textContent

beforeEach(() => {
  vi.mocked(fetchPlatformExperimentAgents).mockResolvedValue([])
})

describe('the phase strip', () => {
  it.each([
    ['draft', 'planning', 'Planning'],
    ['draft', 'revising', 'Planning'],
    ['draft', 'proposed', 'Proposal'],
    ['draft', 'declined', 'Proposal'],
    ['open', 'baseline_measuring', 'Preparing baseline'],
    ['open', 'baseline_required', 'Preparing baseline'],
    ['open', 'budget_below_baseline', 'Preparing baseline'],
    ['open', 'starts_at_ahead', 'Preparing baseline'],
    ['open', 'starting', 'Preparing baseline'],
    ['running', 'agents_starting', 'Running'],
    ['running', 'running', 'Running'],
    ['closed', 'end_time_reached', 'Closed'],
    ['closed', 'agents_not_started', 'Closed'],
    ['closed', 'planner_stopped', 'Closed'],
  ] as const)('stands at one phase, from the status and its reason alone: %s, %s', (status, reason, phase) => {
    show(experiment(status, reason))
    expect(current()).toEqual([phase])
  })

  it('says who closed it early and the reason they gave', () => {
    const closer = { kind: 'user' as const, id: 'u-1', name: 'Ada' }
    const one = show(experiment('closed', 'closed_by_request', { closed_by: closer }))
    expect(sentence()).toBe('It was closed early by Ada.')
    one.unmount()
    show(experiment('closed', 'closed_by_request', { closed_by: closer, close_message: 'the baseline measured the old split' }))
    expect(sentence()).toBe('It was closed early by Ada, who said: the baseline measured the old split')
  })

  it('refuses an experiment that came without the reason every read of one carries', () => {
    vi.spyOn(console, 'error').mockImplementation(() => {})
    expect(() => show({ ...experiment('draft', 'planning'), status_reason: undefined })).toThrow(/came without a status_reason/)
  })
})

describe('whose move it is', () => {
  const waits = (reason: string, who: StatusReason['waits_for'], more: Partial<Draft> = {}) =>
    experiment('open', reason, { baseline_job_id: 'job-base', ...more }, { waits_for: who, message: `It waits: ${reason}.` })
  const strip = () => within(screen.getByRole('region', { name: 'Where this experiment stands' }))

  it('says so prominently when it is the owner\'s, in the API\'s words, with the way to add a researcher', () => {
    show(waits('no_researchers', 'owner'))
    expect(strip().getByText('Your move')).toBeTruthy()
    expect(sentence()).toBe('It waits: no_researchers.')
    expect(strip().getByRole('link', { name: 'Add a researcher' }).getAttribute('href')).toBe('#agents')
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
    const shared = render(<SWRConfig value={{ provider: () => new Map() }}><PhaseStrip pe={waits('no_researchers', 'owner')} readOnly /></SWRConfig>)
    expect(sentence()).toBe('It waits: no_researchers.')
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
  const proposal: Proposal = {
    experiment: {
      name: 'Schedules on tiny transformers',
      description: 'Train the small transformer in train.py and report val_loss.',
      metrics: [{ key: 'val_loss', direction: 'minimize' }],
    },
    budget_accelerator_hours: 6,
    duration_hours: 4,
    baseline_minutes: 30,
    agents: [{}, {}],
    accelerator_type: 'rtx-4090',
    metric_reason: 'Validation loss on held-out text is the goal itself.',
    measurement: 'bench.py scores a split the training never reads.',
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
    expect(screen.getByText('Researcher 2')).toBeTruthy()
    expect(screen.queryAllByRole('textbox')).toHaveLength(0)
    expect(screen.queryAllByRole('spinbutton')).toHaveLength(0)
    for (const name of ['Accept and run', 'Accept now', 'Edit', 'Reject', 'Revise']) expect(screen.queryByRole('button', { name })).toBeNull()
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
