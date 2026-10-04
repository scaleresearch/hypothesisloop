import { render, screen } from '@testing-library/react'
import { SWRConfig } from 'swr'
import { describe, expect, it, vi } from 'vitest'
import { fetchPlatformExperimentsPage } from '@/lib/api'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import ProjectHypothesesPage from '@/app/(app)/projects/[projectId]/hypotheses/page'
import type { Me, PlatformExperiment } from '@/types'

const navigation = vi.hoisted(() => ({ replace: vi.fn() }))

vi.mock('next/navigation', () => ({ useRouter: () => navigation }))
vi.mock('@/lib/api', () => ({ fetchPlatformExperimentsPage: vi.fn() }))

const at = '2026-01-01T00:00:00Z'
const me: Me = {
  kind: 'user',
  user_id: 'u1',
  orgs: [{ id: 'org_a', name: 'Ada', role: 'owner', created_at: at }],
  projects: [{ id: 'prj_1', org_id: 'org_a', name: 'default', created_at: at }],
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

describe('the hypotheses of a project', () => {
  it('open on the newest experiment listed for the project, which a shared one may be', async () => {
    vi.mocked(fetchPlatformExperimentsPage).mockResolvedValue({ items: [{ id: 'pe-x', project_id: 'prj_platform', shared: true } as PlatformExperiment], total: 1 })
    show()
    await vi.waitFor(() => expect(navigation.replace).toHaveBeenCalledExactlyOnceWith('/experiments/pe-x/hypotheses'))
    expect(fetchPlatformExperimentsPage).toHaveBeenCalledWith({ project_id: 'prj_1', sort: '-starts_at', limit: 1 })
  })

  it('say so when the project has no experiment', async () => {
    vi.mocked(fetchPlatformExperimentsPage).mockResolvedValue({ items: [], total: 0 })
    show()
    expect(await screen.findByText(/No experiments yet/)).toBeTruthy()
    expect(navigation.replace).not.toHaveBeenCalled()
  })
})
