import { renderHook } from '@testing-library/react'
import type { ReactNode } from 'react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { ExampleContext, isExample, platformExperimentHref, useListScope, useReadOnly, useRecordScope } from '@/lib/example'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import type { Me } from '@/types'

const navigation = vi.hoisted(() => ({ replace: vi.fn(), search: '' }))

vi.mock('next/navigation', () => ({
  useRouter: () => navigation,
  usePathname: () => '/jobs/j1',
  useSearchParams: () => new URLSearchParams(navigation.search),
}))

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

function wrapper(who: Me, exampleID?: string) {
  const resolved = resolveTenant(who, '')
  if (resolved.state !== 'selected') throw new Error('no tenant')
  return function Wrapper({ children }: { children: ReactNode }) {
    return (
      <TenantContext.Provider value={resolved.tenant}>
        <ExampleContext.Provider value={exampleID ? { id: exampleID } : null}>{children}</ExampleContext.Provider>
      </TenantContext.Provider>
    )
  }
}

beforeEach(() => {
  navigation.search = ''
})

describe('an example', () => {
  it('is a shared platform experiment in a project the caller does not reach', () => {
    expect(isExample(me, { shared: true, project_id: 'prj_platform' })).toBe(true)
    expect(isExample(me, { shared: true, project_id: 'prj_1' })).toBe(false)
    expect(isExample(me, { shared: false, project_id: 'prj_platform' })).toBe(false)
    expect(isExample(staff, { shared: true, project_id: 'prj_platform' })).toBe(false)
  })

  it('opens with the example named, and one of the caller\'s own as any other', () => {
    expect(platformExperimentHref(me, { id: 'pe-x', shared: true, project_id: 'prj_platform' })).toBe('/platform-experiments/pe-x?example=pe-x')
    expect(platformExperimentHref(me, { id: 'pe-1', shared: true, project_id: 'prj_1' })).toBe('/platform-experiments/pe-1')
    expect(platformExperimentHref(staff, { id: 'pe-x', shared: true, project_id: 'prj_platform' })).toBe('/platform-experiments/pe-x')
  })

  it('is read only, and nothing outside one is', () => {
    expect(renderHook(() => useReadOnly(), { wrapper: wrapper(me, 'pe-x') }).result.current).toBe(true)
    expect(renderHook(() => useReadOnly(), { wrapper: wrapper(me) }).result.current).toBe(false)
    expect(renderHook(() => useReadOnly()).result.current).toBe(false)
  })
})

describe('the scope of a list', () => {
  it('is the selected project', () => {
    expect(renderHook(() => useListScope(), { wrapper: wrapper(me) }).result.current).toEqual({ project_id: 'prj_1' })
  })

  it('inside an example is that platform experiment, with no project of the caller\'s', () => {
    expect(renderHook(() => useListScope(), { wrapper: wrapper(me, 'pe-x') }).result.current).toEqual({ platform_experiment_id: 'pe-x' })
  })
})

describe('the scope of an open record', () => {
  it('puts its project in the URL when the caller reaches it', () => {
    const { result } = renderHook(() => useRecordScope({ project_id: 'prj_1', platform_experiment_id: 'pe-1' }), { wrapper: wrapper(me) })
    expect(navigation.replace).toHaveBeenCalledExactlyOnceWith('/jobs/j1?project=prj_1')
    expect(result.current).toBe(true)
  })

  it('puts the example in the URL, and never the project, when the record is another org\'s', () => {
    const { result } = renderHook(() => useRecordScope({ project_id: 'prj_platform', platform_experiment_id: 'pe-x' }), { wrapper: wrapper(me) })
    expect(navigation.replace).toHaveBeenCalledExactlyOnceWith('/jobs/j1?example=pe-x')
    expect(result.current).toBe(false)
  })

  it('is settled once the URL names the example', () => {
    navigation.search = 'example=pe-x'
    const { result } = renderHook(() => useRecordScope({ project_id: 'prj_platform', platform_experiment_id: 'pe-x' }), { wrapper: wrapper(me) })
    expect(navigation.replace).not.toHaveBeenCalled()
    expect(result.current).toBe(true)
  })

  it('takes staff, who reach every project, into the record\'s own project', () => {
    navigation.search = 'example=pe-x'
    renderHook(() => useRecordScope({ project_id: 'prj_platform', platform_experiment_id: 'pe-x' }), { wrapper: wrapper(staff) })
    expect(navigation.replace).toHaveBeenCalledExactlyOnceWith('/jobs/j1?project=prj_platform')
  })

  it('waits for the record', () => {
    const { result } = renderHook(() => useRecordScope(undefined), { wrapper: wrapper(me) })
    expect(navigation.replace).not.toHaveBeenCalled()
    expect(result.current).toBe(true)
  })
})
