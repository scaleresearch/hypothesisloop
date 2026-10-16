import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { fetchHypothesesPage, fetchHypothesisStats, fetchPlatformExperiment, fetchPlatformExperimentsPage } from '@/lib/api'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import ProjectHypothesesPage from '@/app/(app)/projects/[projectId]/hypotheses/page'
import type { HypothesisListItem, Me, PlatformExperiment } from '@/types'

const navigation = vi.hoisted(() => ({ push: vi.fn(), replace: vi.fn(), search: '' }))

vi.mock('next/navigation', () => ({
  useRouter: () => ({ push: navigation.push, replace: navigation.replace }),
  usePathname: () => '/projects/prj_1/hypotheses',
  useSearchParams: () => new URLSearchParams(navigation.search),
}))
vi.mock('@/lib/api', async importActual => ({
  ...(await importActual<typeof import('@/lib/api')>()),
  fetchHypothesesPage: vi.fn(),
  fetchHypothesisStats: vi.fn(),
  fetchPlatformExperiment: vi.fn(),
  fetchPlatformExperimentsPage: vi.fn(),
}))
vi.mock('@/components/platform-experiment-select', () => ({
  PlatformExperimentSelect: ({ value, onChange }: { value: string; onChange: (id: string) => void }) => (
    <select aria-label="Experiment filter" value={value} onChange={e => onChange(e.target.value)}>
      <option value="">All experiments</option>
      <option value="pe-1">Warmup sweep</option>
    </select>
  ),
}))

const at = '2026-01-01T00:00:00Z'
const me: Me = {
  kind: 'user',
  user_id: 'u1',
  orgs: [{ id: 'org_a', name: 'Ada', role: 'owner', created_at: at }],
  projects: [{ id: 'prj_1', org_id: 'org_a', name: 'default', created_at: at }],
}
const experiments: Record<string, PlatformExperiment> = {
  'pe-1': { id: 'pe-1', project_id: 'prj_1', name: 'Warmup sweep', status: 'running', shared: false } as PlatformExperiment,
  'pe-2': { id: 'pe-2', project_id: 'prj_1', name: 'Tokenizer study', status: 'closed', shared: false } as PlatformExperiment,
}
const hypothesis = (id: string, pe: string, text: string, more: Partial<HypothesisListItem> = {}): HypothesisListItem => ({
  id, platform_experiment_id: pe, text, status: 'open', agent_id: 'agent-1',
  actor: { kind: 'agent', id: 'agent-1', name: 'ada-agent' },
  created_at: at, status_changed_at: null, status_actor: null,
  builds_on: [pe], child_count: 0, finding_count: 0, comment_count: 0, ...more,
})

function show() {
  const resolved = resolveTenant(me, 'prj_1')
  if (resolved.state !== 'selected') throw new Error('no tenant')
  return render(
    <SWRConfig value={{ provider: () => new Map() }}>
      <TenantContext.Provider value={resolved.tenant}><ProjectHypothesesPage /></TenantContext.Provider>
    </SWRConfig>,
  )
}

beforeEach(() => {
  navigation.push.mockReset()
  navigation.replace.mockReset()
  navigation.search = ''
  vi.mocked(fetchPlatformExperimentsPage).mockReset().mockResolvedValue({ items: Object.values(experiments), total: 2 })
  vi.mocked(fetchPlatformExperiment).mockReset().mockImplementation(async id => experiments[id])
  vi.mocked(fetchHypothesisStats).mockReset().mockImplementation(async pe => pe === 'pe-1'
    ? { total: 5, by_status: { open: 3, confirmed: 1, refuted: 1 } }
    : { total: 0, by_status: {} })
  vi.mocked(fetchHypothesesPage).mockReset().mockResolvedValue({
    items: [
      hypothesis('h1', 'pe-1', 'A longer warmup lowers the loss', { status: 'confirmed', status_changed_at: '2026-01-02T00:00:00Z' }),
      hypothesis('h2', 'pe-2', 'A person idea', { agent_id: '', actor: { kind: 'user', id: 'u1', name: 'Ada' } }),
    ],
    total: 40,
  })
})

describe('the hypotheses of a project', () => {
  it('list each experiment with its hypotheses by status and a link to its graph', async () => {
    show()
    const row = (await screen.findByRole('link', { name: 'Warmup sweep' })).closest('tr')!
    await vi.waitFor(() => expect(within(row).getAllByRole('cell').map(c => c.textContent)).toEqual(
      ['Warmup sweep', 'running', '5', '3', '1', '1', '0', 'Graph'],
    ))
    expect(within(row).getByRole('link', { name: 'Warmup sweep' }).getAttribute('href')).toBe('/experiments/pe-1/hypotheses')
    expect(within(row).getByRole('link', { name: 'Graph of Warmup sweep' }).getAttribute('href')).toBe('/experiments/pe-1/hypotheses')
    expect(fetchPlatformExperimentsPage).toHaveBeenCalledWith({ project_id: 'prj_1', sort: '-starts_at', limit: 10, offset: 0 })
  })

  it('list every experiment\'s hypotheses in one table, a page at a time, each row opening it in its experiment', async () => {
    show()
    const claim = await screen.findByText('A longer warmup lowers the loss')
    const row = claim.closest('tr')!
    expect(within(row).getByText('Warmup sweep')).toBeTruthy()
    expect(within(row).getByText('confirmed')).toBeTruthy()
    expect(within(row).getByText('ada-agent')).toBeTruthy()
    expect(within(screen.getByText('A person idea').closest('tr')!).getByText('Ada')).toBeTruthy()
    expect(fetchHypothesesPage).toHaveBeenCalledWith({ project_id: 'prj_1', limit: 25, offset: 0 })

    await userEvent.click(row)
    expect(navigation.push).toHaveBeenCalledWith('/experiments/pe-1/hypotheses/h1')

    await userEvent.click(screen.getAllByRole('button', { name: /next/i }).at(-1)!)
    await vi.waitFor(() => expect(fetchHypothesesPage).toHaveBeenLastCalledWith({ project_id: 'prj_1', limit: 25, offset: 25 }))
  })

  it('ask the server for the experiment, status and words the address names', async () => {
    navigation.search = 'experiment=pe-1&status=refuted&q=warm'
    show()
    await vi.waitFor(() => expect(fetchHypothesesPage).toHaveBeenCalledWith({
      project_id: 'prj_1', platform_experiment_id: 'pe-1', status: 'refuted', q: 'warm', limit: 25, offset: 0,
    }))
    // The experiment is the same on every row, so its column goes.
    const table = (await screen.findByRole('columnheader', { name: 'Claim' })).closest('table')!
    expect(within(table).getAllByRole('columnheader').map(th => th.textContent)).toEqual(['Claim', 'Status', 'Owner', 'Last activity'])
  })

  it('put each filter in the address', async () => {
    show()
    await screen.findByText('A longer warmup lowers the loss')
    await userEvent.click(within(screen.getByRole('group', { name: 'Status' })).getByRole('button', { name: 'Refuted' }))
    expect(navigation.push).toHaveBeenLastCalledWith('/projects/prj_1/hypotheses?status=refuted')
    await userEvent.selectOptions(screen.getByLabelText('Experiment filter'), 'pe-1')
    expect(navigation.push).toHaveBeenLastCalledWith('/projects/prj_1/hypotheses?experiment=pe-1')
    await userEvent.type(screen.getByLabelText('Search hypotheses'), 'w')
    expect(navigation.push).toHaveBeenLastCalledWith('/projects/prj_1/hypotheses?q=w')
  })

  it('say when nothing matches the filters', async () => {
    navigation.search = 'q=nothing'
    vi.mocked(fetchHypothesesPage).mockResolvedValue({ items: [], total: 0 })
    show()
    expect(await screen.findByText('No hypotheses match "nothing".')).toBeTruthy()
  })

  it('lead to creating an experiment when the project has none', async () => {
    vi.mocked(fetchPlatformExperimentsPage).mockResolvedValue({ items: [], total: 0 })
    show()
    expect(await screen.findByText(/No experiments yet/)).toBeTruthy()
    expect(screen.getByRole('link', { name: 'Create an experiment' }).getAttribute('href')).toBe('/projects/prj_1/experiments')
    expect(fetchHypothesesPage).not.toHaveBeenCalled()
    expect(navigation.replace).not.toHaveBeenCalled()
  })
})
