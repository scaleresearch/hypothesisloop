import { renderHook } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { useRecordProject } from '@/lib/query-state'

const navigation = vi.hoisted(() => ({ replace: vi.fn(), search: '' }))

vi.mock('next/navigation', () => ({
  useRouter: () => navigation,
  usePathname: () => '/jobs/j1',
  useSearchParams: () => new URLSearchParams(navigation.search),
}))

beforeEach(() => {
  navigation.search = ''
})

describe('the project of an open record', () => {
  it('replaces the one the URL names, keeping the rest of the query', () => {
    navigation.search = 'project=prj_other&tab=logs'
    renderHook(() => useRecordProject('prj_1'))
    expect(navigation.replace).toHaveBeenCalledExactlyOnceWith('/jobs/j1?project=prj_1&tab=logs')
  })

  it('is added when the URL names none', () => {
    renderHook(() => useRecordProject('prj_1'))
    expect(navigation.replace).toHaveBeenCalledExactlyOnceWith('/jobs/j1?project=prj_1')
  })

  it('leaves the URL alone while the record loads and when it already agrees', () => {
    navigation.search = 'project=prj_1'
    renderHook(() => useRecordProject(undefined))
    renderHook(() => useRecordProject('prj_1'))
    expect(navigation.replace).not.toHaveBeenCalled()
  })
})
