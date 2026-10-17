import { renderHook, waitFor } from '@testing-library/react'
import type { ReactNode } from 'react'
import { SWRConfig } from 'swr'
import { describe, expect, it, vi } from 'vitest'
import { fetchPlatformExperiment } from '@/lib/api/platform-experiments'
import { isReadOnly, listProject, ReadOnlyProvider, useExperimentAccess, useReadOnly } from '@/lib/read-only'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import type { Me, PlatformExperiment } from '@/types/api'

vi.mock('@/lib/api/platform-experiments', () => ({ fetchPlatformExperiment: vi.fn() }))

const at = '2026-01-01T00:00:00Z'
const me: Me = {
  kind: 'user',
  user_id: 'u1',
  orgs: [{ id: 'org_a', name: 'Ada', role: 'owner', created_at: at }],
  projects: [{ id: 'prj_1', org_id: 'org_a', name: 'default', created_at: at }],
}
// Staff are listed every org and project.
const staff: Me = {
  kind: 'staff',
  orgs: [...me.orgs, { id: 'org_p', name: 'Platform', created_at: at }],
  projects: [...me.projects, { id: 'prj_platform', org_id: 'org_p', name: 'default', created_at: at }],
}
const project = me.projects[0]

function wrapper(who: Me) {
  const resolved = resolveTenant(who, '')
  if (resolved.state !== 'selected') throw new Error('no tenant')
  return function Wrapper({ children }: { children: ReactNode }) {
    return (
      <SWRConfig value={{ provider: () => new Map(), shouldRetryOnError: false }}>
        <TenantContext.Provider value={resolved.tenant}>{children}</TenantContext.Provider>
      </SWRConfig>
    )
  }
}

function experiment(projectID: string, shared: boolean) {
  return { id: 'pe-x', project_id: projectID, shared } as PlatformExperiment
}

describe('read only', () => {
  it('is a shared platform experiment in a project the caller does not reach', () => {
    expect(isReadOnly(me, { shared: true, project_id: 'prj_platform' })).toBe(true)
    expect(isReadOnly(me, { shared: true, project_id: 'prj_1' })).toBe(false)
    expect(isReadOnly(me, { shared: false, project_id: 'prj_1' })).toBe(false)
    expect(isReadOnly(staff, { shared: true, project_id: 'prj_platform' })).toBe(false)
  })

  it('is what a page says of its experiment, and nothing is read only without one', () => {
    const inside = ({ children }: { children: ReactNode }) => <ReadOnlyProvider value>{children}</ReadOnlyProvider>
    expect(renderHook(() => useReadOnly(), { wrapper: inside }).result.current).toBe(true)
    expect(renderHook(() => useReadOnly()).result.current).toBe(false)
  })
})

describe('what a page may do in its platform experiment', () => {
  it('is everything when the page is about no single one', () => {
    const { result } = renderHook(() => useExperimentAccess(undefined), { wrapper: wrapper(me) })
    expect(result.current).toEqual({ readOnly: false, known: true })
    expect(fetchPlatformExperiment).not.toHaveBeenCalled()
    expect(listProject(project, result.current)).toEqual({ project_id: 'prj_1' })
  })

  it('is nothing until the experiment has been read, then reading for a shared one of another org', async () => {
    vi.mocked(fetchPlatformExperiment).mockResolvedValue(experiment('prj_platform', true))
    const { result } = renderHook(() => useExperimentAccess('pe-x'), { wrapper: wrapper(me) })
    expect(result.current).toEqual({ pe: undefined, readOnly: true, known: false })
    await waitFor(() => expect(result.current.known).toBe(true))
    expect(result.current.readOnly).toBe(true)
    // Its lists are asked for by the experiment alone: the caller's project would match nothing.
    expect(listProject(project, result.current)).toEqual({})
  })

  it('is everything in one of the caller\'s own, shared or not, and its lists stay in the project', async () => {
    vi.mocked(fetchPlatformExperiment).mockResolvedValue(experiment('prj_1', true))
    const { result } = renderHook(() => useExperimentAccess('pe-x'), { wrapper: wrapper(me) })
    await waitFor(() => expect(result.current.known).toBe(true))
    expect(result.current.readOnly).toBe(false)
    expect(listProject(project, result.current)).toEqual({ project_id: 'prj_1' })
  })

  it('is everything for staff, who reach every project', async () => {
    vi.mocked(fetchPlatformExperiment).mockResolvedValue(experiment('prj_platform', true))
    const { result } = renderHook(() => useExperimentAccess('pe-x'), { wrapper: wrapper(staff) })
    await waitFor(() => expect(result.current.known).toBe(true))
    expect(result.current.readOnly).toBe(false)
  })

  it('is nothing in one that cannot be read, with lists left in the caller\'s project', async () => {
    vi.mocked(fetchPlatformExperiment).mockRejectedValue(new Error('platform experiment not found'))
    const { result } = renderHook(() => useExperimentAccess('pe-x'), { wrapper: wrapper(me) })
    await waitFor(() => expect(result.current.known).toBe(true))
    expect(result.current.readOnly).toBe(true)
    expect(listProject(project, result.current)).toEqual({ project_id: 'prj_1' })
  })
})
