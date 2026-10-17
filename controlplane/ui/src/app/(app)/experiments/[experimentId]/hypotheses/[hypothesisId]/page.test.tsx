import { render, screen, within } from '@testing-library/react'
import { SWRConfig } from 'swr'
import { describe, expect, it, vi } from 'vitest'
import { fetchPlatformExperimentAgents } from '@/lib/api/agents'
import { fetchHypothesis } from '@/lib/api/hypotheses'
import { fetchPlatformExperiment } from '@/lib/api/platform-experiments'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import HypothesisDetailPage from '@/app/(app)/experiments/[experimentId]/hypotheses/[hypothesisId]/page'
import type { HypothesisWithJobs, Me, PlatformExperiment } from '@/types/api'

vi.mock('next/navigation', () => ({
  useRouter: () => ({ push: vi.fn(), replace: vi.fn() }),
  usePathname: () => '/experiments/pe-x/hypotheses/hyp-1',
  useSearchParams: () => new URLSearchParams(),
}))
vi.mock('@/lib/api/agents', () => ({ fetchPlatformExperimentAgents: vi.fn() }))
vi.mock('@/lib/api/hypotheses', () => ({ fetchHypothesis: vi.fn(), addHumanHypothesisComment: vi.fn(), addHumanHypothesisLink: vi.fn(), searchHypotheses: vi.fn() }))
vi.mock('@/lib/api/platform-experiments', () => ({ fetchPlatformExperiment: vi.fn() }))

const at = '2026-01-01T00:00:00Z'
const me: Me = {
  kind: 'user',
  user_id: 'u1',
  orgs: [{ id: 'org_a', name: 'Ada', role: 'owner', created_at: at }],
  projects: [{ id: 'prj_1', org_id: 'org_a', name: 'default', created_at: at }],
}
const hypothesis = {
  id: 'hyp-1', platform_experiment_id: 'pe-x', text: 'warmup helps', status: 'open', agent_id: 'ag-1',
  actor: { kind: 'agent', id: 'ag-1', name: 'Researcher 1' }, status_changed_at: null, created_at: at,
  links: { out: [], in: [] }, jobs: [], job_count: 0, findings: [], finding_count: 0, comments: [], comment_count: 0,
  implementations: [],
} as unknown as HypothesisWithJobs

function show(answer: Partial<HypothesisWithJobs>, experimentId = 'pe-x') {
  vi.mocked(fetchHypothesis).mockResolvedValue({ ...hypothesis, ...answer })
  vi.mocked(fetchPlatformExperiment).mockResolvedValue({ id: 'pe-x', project_id: 'prj_1', name: 'Vision benchmark' } as PlatformExperiment)
  vi.mocked(fetchPlatformExperimentAgents).mockResolvedValue([{ agent_id: 'ag-1', name: 'Researcher 1' }, { agent_id: 'ag-2', name: 'Researcher 2' }] as never)
  const resolved = resolveTenant(me, '')
  if (resolved.state !== 'selected') throw new Error('no tenant')
  return render(
    <SWRConfig value={{ provider: () => new Map() }}>
      <TenantContext.Provider value={resolved.tenant}><HypothesisDetailPage params={{ experimentId, hypothesisId: 'hyp-1' }} /></TenantContext.Provider>
    </SWRConfig>,
  )
}

describe('the implementations of a hypothesis', () => {
  it('are one branch per agent that ran it, each with its newest commit, and the commit a new one starts from', async () => {
    show({
      base_ref: 'https://github.com/acme/widgets@aaaaaaaabbbbbbbb',
      implementations: [
        { agent_id: 'ag-1', branch: 'hl/pe-x/warmup-ag-1', code_ref: 'https://github.com/acme/widgets@1111111122222222' },
        { agent_id: 'ag-2', branch: 'hl/pe-x/warmup-ag-2', code_ref: 'https://github.com/acme/widgets@3333333344444444' },
      ],
    })
    const row = within((await screen.findByRole('link', { name: 'warmup-ag-2' })).closest('tr')!)
    expect(row.getByText('Researcher 2').getAttribute('title')).toBe('ag-2')
    expect(row.getByRole('link', { name: 'warmup-ag-2' }).getAttribute('href')).toBe('https://github.com/acme/widgets/tree/hl/pe-x/warmup-ag-2')
    expect(row.getByRole('link', { name: '33333333' }).getAttribute('href')).toBe('https://github.com/acme/widgets/commit/3333333344444444')
    expect(screen.getByRole('link', { name: 'warmup-ag-1' })).toBeTruthy()
    const base = screen.getByRole('link', { name: 'aaaaaaaa' })
    expect(base.getAttribute('href')).toBe('https://github.com/acme/widgets/commit/aaaaaaaabbbbbbbb')
    expect(base.closest('h2')!.textContent).toBe('Implementationsa new one starts from aaaaaaaa')
  })

  it('are none before a job of it ran, and no commit to start from before a baseline is declared', async () => {
    show({})
    expect(await screen.findByText(/No agent has run this hypothesis yet/)).toBeTruthy()
    expect(screen.queryByText(/a new one starts from/)).toBeNull()
  })
})

describe('a link to a hypothesis', () => {
  it('opens it only under the experiment it belongs to', async () => {
    show({}, 'pe-other')
    expect(await screen.findByText(/it is not in this experiment/)).toBeTruthy()
    expect(screen.getByRole('link', { name: '← Hypothesis graph' }).getAttribute('href')).toBe('/experiments/pe-other/hypotheses')
  })
})
