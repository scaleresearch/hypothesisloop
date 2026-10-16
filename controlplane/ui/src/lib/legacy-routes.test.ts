import { describe, expect, it } from 'vitest'
import { movedTo, recordPath } from '@/lib/legacy-routes'

const moved = (url: string) => {
  const u = new URL(url, 'http://ui')
  return movedTo(u.pathname, u.searchParams)
}

describe('the pages that moved', () => {
  it('go to the experiment they name, keeping their view state', () => {
    expect(moved('/platform-experiments/pe-1')).toEqual({ path: '/experiments/pe-1' })
    expect(moved('/hypotheses?pe=pe-1&view=outline&focus=h1')).toEqual({ path: '/experiments/pe-1/hypotheses?view=outline&focus=h1' })
    expect(moved('/jobs?platform_experiment_id=pe-1&status=RUNNING')).toEqual({ path: '/experiments/pe-1/jobs?status=RUNNING' })
    expect(moved('/agents?platform_experiment_id=pe-1')).toEqual({ path: '/experiments/pe-1/agents' })
  })

  it('go through the experiment a job or hypothesis belongs to', () => {
    expect(moved('/jobs/j1')).toEqual({ record: 'job', id: 'j1' })
    expect(moved('/hypotheses/h1')).toEqual({ record: 'hypothesis', id: 'h1' })
    expect(recordPath('job', 'j1', 'pe-1')).toBe('/experiments/pe-1/jobs/j1')
    expect(recordPath('hypothesis', 'h1', 'pe-1')).toBe('/experiments/pe-1/hypotheses/h1')
  })

  it('go to the project a link named, or else to the person\'s own through the console', () => {
    expect(moved('/platform-experiments?project=prj_1&status=OPEN')).toEqual({ path: '/projects/prj_1/experiments?status=OPEN' })
    expect(moved('/statistics?project=prj_1')).toEqual({ path: '/projects/prj_1/statistics' })
    expect(moved('/platform-experiments?q=vision')).toEqual({ path: '/console?to=experiments&q=vision' })
    expect(moved('/hypotheses')).toEqual({ path: '/console?to=hypotheses' })
    expect(moved('/jobs')).toEqual({ path: '/console?to=jobs' })
    expect(moved('/agents')).toEqual({ path: '/console?to=agents' })
    expect(moved('/statistics')).toEqual({ path: '/console?to=statistics' })
    expect(moved('/cluster?project=prj_1')).toEqual({ path: '/console?to=clusters' })
    expect(moved('/settings')).toEqual({ path: '/console?to=settings' })
  })

  it('send every bare section the nav names to the person\'s own project, none of them a 404', () => {
    expect(moved('/experiments')).toEqual({ path: '/console?to=experiments' })
    expect(moved('/hypotheses')).toEqual({ path: '/console?to=hypotheses' })
    expect(moved('/jobs')).toEqual({ path: '/console?to=jobs' })
    expect(moved('/clusters')).toEqual({ path: '/console?to=clusters' })
    expect(moved('/statistics')).toEqual({ path: '/console?to=statistics' })
    expect(moved('/settings')).toEqual({ path: '/console?to=settings' })
    expect(moved('/experiments?project=prj_1&status=open')).toEqual({ path: '/projects/prj_1/experiments?status=open' })
  })

  it('leave every current page alone', () => {
    for (const path of ['/settings/repository', '/console', '/experiments/pe-1', '/experiments/pe-1/hypotheses', '/clusters/c1', '/projects/prj_1/jobs', '/orgs/o1/settings', '/jobs/j1/logs', '/']) {
      expect(moved(path)).toBeNull()
    }
  })
})
