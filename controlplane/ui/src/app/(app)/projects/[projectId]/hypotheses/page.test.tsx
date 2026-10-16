import { render, screen } from '@testing-library/react'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { fetchHypothesisStats } from '@/lib/api/hypotheses'
import { fetchPlatformExperiment, fetchPlatformExperimentsPage } from '@/lib/api/platform-experiments'
import type { Me } from '@/types/accounts'
import type { PlatformExperiment } from '@/types/platform-experiments'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import ProjectHypothesesPage from '@/app/(app)/projects/[projectId]/hypotheses/page'

const navigation = vi.hoisted(() => ({ push: vi.fn(), replace: vi.fn(), search: '' }))

vi.mock('next/navigation', () => ({
  useRouter: () => ({ push: navigation.push, replace: navigation.replace }),
  usePathname: () => '/projects/prj_1/hypotheses',
  useSearchParams: () => new URLSearchParams(navigation.search),
}))
vi.mock('@/lib/api/hypotheses', async importActual => ({
  ...(await importActual<typeof import('@/lib/api/hypotheses')>()),
  fetchHypothesisStats: vi.fn(),
}))
vi.mock('@/lib/api/platform-experiments', async importActual => ({
  ...(await importActual<typeof import('@/lib/api/platform-experiments')>()),
  fetchPlatformExperiment: vi.fn(), fetchPlatformExperimentsPage: vi.fn(),
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
})

describe('the hypotheses of a project', () => {
  it('lead to creating an experiment when the project has none', async () => {
    vi.mocked(fetchPlatformExperimentsPage).mockResolvedValue({ items: [], total: 0 })
    show()
    expect(await screen.findByText(/No experiments yet/)).toBeTruthy()
    expect(screen.getByRole('link', { name: 'Create an experiment' }).getAttribute('href')).toBe('/projects/prj_1/experiments')
    expect(navigation.replace).not.toHaveBeenCalled()
  })
})
