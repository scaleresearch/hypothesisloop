import { renderHook } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { exampleHref, useExampleParam, useProjectHref, useRecordExample, useRecordProject } from '@/lib/query-state'

const navigation = vi.hoisted(() => ({ replace: vi.fn(), search: '', pathname: '/jobs/j1' }))

vi.mock('next/navigation', () => ({
  useRouter: () => navigation,
  usePathname: () => navigation.pathname,
  useSearchParams: () => new URLSearchParams(navigation.search),
}))

beforeEach(() => {
  navigation.search = ''
  navigation.pathname = '/jobs/j1'
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

describe('the example of an open record', () => {
  it('is added to the URL beside the caller\'s own project', () => {
    navigation.search = 'project=prj_1&tab=logs'
    const { result } = renderHook(() => useRecordExample('pe-x'))
    expect(navigation.replace).toHaveBeenCalledExactlyOnceWith('/jobs/j1?project=prj_1&tab=logs&example=pe-x')
    expect(result.current).toBe(false)
  })

  it('leaves the URL alone while the record loads and when it already agrees', () => {
    navigation.search = 'example=pe-x'
    renderHook(() => useRecordExample(undefined))
    const { result } = renderHook(() => useRecordExample('pe-x'))
    expect(navigation.replace).not.toHaveBeenCalled()
    expect(result.current).toBe(true)
  })

  it('is dropped when the record turns out to be the caller\'s own', () => {
    navigation.search = 'project=prj_1&example=pe-x'
    renderHook(() => useRecordProject('prj_1'))
    expect(navigation.replace).toHaveBeenCalledExactlyOnceWith('/jobs/j1?project=prj_1')
  })
})

describe('links inside an example', () => {
  it('carry it to the pages that show an experiment\'s records, and to no other', () => {
    navigation.search = 'project=prj_1&example=pe-x'
    const { result } = renderHook(() => useProjectHref())
    expect(result.current('/jobs/j2')).toBe('/jobs/j2?project=prj_1&example=pe-x')
    expect(result.current('/jobs?platform_experiment_id=pe-x&status=RUNNING')).toBe('/jobs?platform_experiment_id=pe-x&status=RUNNING&project=prj_1&example=pe-x')
    expect(result.current('/hypotheses?pe=pe-x')).toBe('/hypotheses?pe=pe-x&project=prj_1&example=pe-x')
    expect(result.current('/hypotheses/hyp-1')).toBe('/hypotheses/hyp-1?project=prj_1&example=pe-x')
    expect(result.current('/platform-experiments/pe-x')).toBe('/platform-experiments/pe-x?project=prj_1&example=pe-x')
    expect(result.current('/platform-experiments')).toBe('/platform-experiments?project=prj_1')
    expect(result.current('/examples')).toBe('/examples?project=prj_1')
    expect(result.current('/agents')).toBe('/agents?project=prj_1')
    expect(result.current('/settings')).toBe('/settings?project=prj_1')
  })

  it('drop it when they lead out', () => {
    navigation.search = 'project=prj_1&example=pe-x'
    const { result } = renderHook(() => useProjectHref(true))
    expect(result.current('/jobs')).toBe('/jobs?project=prj_1')
    expect(result.current('/hypotheses')).toBe('/hypotheses?project=prj_1')
  })

  it('carry none outside one', () => {
    navigation.search = 'project=prj_1'
    const { result } = renderHook(() => useProjectHref())
    expect(result.current('/jobs/j2')).toBe('/jobs/j2?project=prj_1')
  })

  it('open an example on its own page with the example named', () => {
    expect(exampleHref('pe-x')).toBe('/platform-experiments/pe-x?example=pe-x')
  })

  it('name no example on a page that shows none', () => {
    navigation.search = 'example=pe-x'
    expect(renderHook(() => useExampleParam()).result.current).toBe('pe-x')
    for (const pathname of ['/agents', '/settings', '/platform-experiments', '/examples', '/console']) {
      navigation.pathname = pathname
      expect(renderHook(() => useExampleParam()).result.current).toBe('')
    }
    for (const pathname of ['/jobs', '/hypotheses', '/hypotheses/hyp-1', '/platform-experiments/pe-x']) {
      navigation.pathname = pathname
      expect(renderHook(() => useExampleParam()).result.current).toBe('pe-x')
    }
  })
})
