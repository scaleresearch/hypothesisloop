import { renderHook, waitFor } from '@testing-library/react'
import type { ReactNode } from 'react'
import { SWRConfig } from 'swr'
import { describe, expect, it, vi } from 'vitest'
import { fetchPlatformExperiment } from '@/lib/api/platform-experiments'
import { listProject, useExperimentAccess } from '@/lib/experiment-actions'
import type { PlatformExperiment, Project } from '@/types/api'

vi.mock('@/lib/api/platform-experiments', () => ({ fetchPlatformExperiment: vi.fn() }))

const project = { id: 'prj_1', org_id: 'org_a', name: 'default', created_at: '2026-01-01T00:00:00Z' } as Project

function wrapper({ children }: { children: ReactNode }) {
  return <SWRConfig value={{ provider: () => new Map(), shouldRetryOnError: false }}>{children}</SWRConfig>
}

function experiment(projectID: string) {
  return { id: 'pe-x', project_id: projectID, shared: true, editable: [], actions: [] } as unknown as PlatformExperiment
}

describe('the experiment a page is about', () => {
  it('is none when the page names none, and its lists stay in the project', () => {
    const { result } = renderHook(() => useExperimentAccess(undefined), { wrapper })
    expect(result.current).toEqual({ known: true })
    expect(fetchPlatformExperiment).not.toHaveBeenCalled()
    expect(listProject(project, result.current)).toEqual({ project_id: 'prj_1' })
  })

  it('is known once read, and the lists of one in another project are asked for by it alone', async () => {
    vi.mocked(fetchPlatformExperiment).mockResolvedValue(experiment('prj_platform'))
    const { result } = renderHook(() => useExperimentAccess('pe-x'), { wrapper })
    expect(result.current).toEqual({ pe: undefined, known: false })
    await waitFor(() => expect(result.current.known).toBe(true))
    expect(listProject(project, result.current)).toEqual({})
  })

  it('keeps the lists of one in the selected project in it', async () => {
    vi.mocked(fetchPlatformExperiment).mockResolvedValue(experiment('prj_1'))
    const { result } = renderHook(() => useExperimentAccess('pe-x'), { wrapper })
    await waitFor(() => expect(result.current.known).toBe(true))
    expect(listProject(project, result.current)).toEqual({ project_id: 'prj_1' })
  })

  it('is known when it cannot be read, with lists left in the caller\'s project', async () => {
    vi.mocked(fetchPlatformExperiment).mockRejectedValue(new Error('platform experiment not found'))
    const { result } = renderHook(() => useExperimentAccess('pe-x'), { wrapper })
    await waitFor(() => expect(result.current.known).toBe(true))
    expect(listProject(project, result.current)).toEqual({ project_id: 'prj_1' })
  })
})
