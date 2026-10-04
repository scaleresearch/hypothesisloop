import { describe, expect, it } from 'vitest'
import { hypothesisPath, jobPath, sectionOf, sectionPath, withQuery } from '@/lib/routes'

const project = { id: 'prj_1', org_id: 'org_1', name: 'default', created_at: '2026-01-01T00:00:00Z' }

describe('routes', () => {
  it('nest a record under the experiment it belongs to', () => {
    expect(jobPath('pe-1', 'j1')).toBe('/experiments/pe-1/jobs/j1')
    expect(hypothesisPath('pe-1', 'h1')).toBe('/experiments/pe-1/hypotheses/h1')
  })

  it('put the pages of a project under it, and the clusters and settings under its org', () => {
    expect(sectionPath('jobs', project)).toBe('/projects/prj_1/jobs')
    expect(sectionPath('clusters', project)).toBe('/orgs/org_1/clusters')
    expect(sectionPath('settings', project)).toBe('/orgs/org_1/settings')
  })

  it('carry view state in the query and leave empty values out', () => {
    expect(withQuery('/experiments/pe-1/jobs', { status: 'RUNNING', agent: '' })).toBe('/experiments/pe-1/jobs?status=RUNNING')
    expect(withQuery('/experiments/pe-1/jobs', { status: undefined })).toBe('/experiments/pe-1/jobs')
  })

  it('place a page in the section that lists it', () => {
    expect(sectionOf('/projects/prj_1/statistics')).toBe('statistics')
    expect(sectionOf('/orgs/org_1/clusters')).toBe('clusters')
    expect(sectionOf('/experiments/pe-1')).toBe('experiments')
    expect(sectionOf('/experiments/pe-1/jobs/j1')).toBe('jobs')
    expect(sectionOf('/experiments/pe-1/hypotheses')).toBe('hypotheses')
    expect(sectionOf('/console')).toBeUndefined()
  })
})
