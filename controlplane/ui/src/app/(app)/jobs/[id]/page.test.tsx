import { render, screen } from '@testing-library/react'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { fetchExperiment, fetchExperimentLogs, fetchExperimentMetrics } from '@/lib/api'
import { resolveTenant, TenantContext } from '@/lib/tenant'
import JobDetailPage from '@/app/(app)/jobs/[id]/page'
import type { Experiment, Me } from '@/types'

const navigation = vi.hoisted(() => ({ search: '', replace: vi.fn() }))

vi.mock('next/navigation', () => ({
  useRouter: () => navigation,
  usePathname: () => '/jobs/job-1',
  useSearchParams: () => new URLSearchParams(navigation.search),
}))
vi.mock('@/lib/api', () => ({ fetchExperiment: vi.fn(), fetchExperimentLogs: vi.fn(), fetchExperimentMetrics: vi.fn() }))
vi.mock('@/components/platform-experiment-name', () => ({ PlatformExperimentName: () => <>Vision benchmark</> }))

const at = '2026-01-01T00:00:00Z'
const me: Me = {
  kind: 'user',
  user_id: 'u1',
  orgs: [{ id: 'org_a', name: 'Ada', role: 'owner', created_at: at }],
  projects: [{ id: 'prj_1', org_id: 'org_a', name: 'default', created_at: at }],
}

function job(projectID: string, platformExperimentID: string): Experiment {
  return {
    id: 'job-1', agent_id: 'ag-1', platform_experiment_id: platformExperimentID, project_id: projectID, hypothesis_id: 'hyp-1',
    status: 'COMPLETED', capacity_tier: 'guaranteed', created_at: at, updated_at: at,
    accelerator_type: 'nvidia.com/gpu.product=NVIDIA-L40', accelerator_count: 1, hypothesis: 'a claim', env: {}, priority_score: 0,
  } as Experiment
}

function show(search: string) {
  navigation.search = search
  const resolved = resolveTenant(me, '')
  if (resolved.state !== 'selected') throw new Error('no tenant')
  return render(
    <SWRConfig value={{ provider: () => new Map() }}>
      <TenantContext.Provider value={resolved.tenant}><JobDetailPage params={{ id: 'job-1' }} /></TenantContext.Provider>
    </SWRConfig>,
  )
}

beforeEach(() => {
  vi.mocked(fetchExperimentLogs).mockResolvedValue(['epoch 1'])
  vi.mocked(fetchExperimentMetrics).mockResolvedValue([])
})

describe('a job opened by its id', () => {
  it('of an example names the example in the URL, never the other org\'s project, and waits for it', async () => {
    vi.mocked(fetchExperiment).mockResolvedValue(job('prj_platform', 'pe-x'))
    show('project=prj_1')
    await vi.waitFor(() => expect(navigation.replace).toHaveBeenCalledExactlyOnceWith('/jobs/job-1?project=prj_1&example=pe-x'))
    expect(screen.queryByText('Job Details')).toBeNull()
  })

  it('of an example reads on inside it', async () => {
    vi.mocked(fetchExperiment).mockResolvedValue(job('prj_platform', 'pe-x'))
    show('project=prj_1&example=pe-x')
    expect(await screen.findByText('Job Details')).toBeTruthy()
    expect(await screen.findByText('epoch 1')).toBeTruthy()
    expect(navigation.replace).not.toHaveBeenCalled()
    expect(screen.getByRole('link', { name: /This experiment's jobs/ }).getAttribute('href')).toBe('/jobs?platform_experiment_id=pe-x&project=prj_1&example=pe-x')
    expect(screen.getByRole('link', { name: 'Vision benchmark' }).getAttribute('href')).toBe('/platform-experiments/pe-x?project=prj_1&example=pe-x')
    expect(screen.getByRole('link', { name: 'View hypothesis →' }).getAttribute('href')).toBe('/hypotheses/hyp-1?project=prj_1&example=pe-x')
  })

  it('of the caller\'s own puts its project in the URL and leaves any example', async () => {
    vi.mocked(fetchExperiment).mockResolvedValue(job('prj_1', 'pe-1'))
    show('example=pe-x')
    expect(await screen.findByText('Job Details')).toBeTruthy()
    expect(navigation.replace).toHaveBeenCalledExactlyOnceWith('/jobs/job-1?project=prj_1')
  })
})
