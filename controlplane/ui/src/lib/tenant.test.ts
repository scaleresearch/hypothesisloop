import { describe, expect, it } from 'vitest'
import { resolveTenant } from '@/lib/tenant'
import type { Me } from '@/types'

const at = '2026-01-01T00:00:00Z'

const twoOrgs: Me = {
  kind: 'user',
  user_id: 'u1',
  name: 'Ada',
  orgs: [
    { id: 'org_a', name: 'Ada', role: 'owner', created_at: at },
    { id: 'org_b', name: 'Lab', role: 'member', created_at: at },
  ],
  projects: [
    { id: 'prj_b1', org_id: 'org_b', name: 'default', created_at: at },
    { id: 'prj_a1', org_id: 'org_a', name: 'default', created_at: at },
    { id: 'prj_a2', org_id: 'org_a', name: 'second', created_at: at },
  ],
}

describe('resolveTenant', () => {
  it('lands a caller with no project in the URL on the first project of their first org', () => {
    const resolved = resolveTenant(twoOrgs, '')
    expect(resolved).toMatchObject({ state: 'selected', tenant: { org: { id: 'org_a' }, project: { id: 'prj_a1' } } })
  })

  it('selects the project the URL names and the org it belongs to', () => {
    const resolved = resolveTenant(twoOrgs, 'prj_b1')
    expect(resolved).toMatchObject({ state: 'selected', tenant: { org: { id: 'org_b' }, project: { id: 'prj_b1' } } })
  })

  it('lets an owner administer and a member not', () => {
    expect(resolveTenant(twoOrgs, 'prj_a2')).toMatchObject({ tenant: { administers: true, staff: false } })
    expect(resolveTenant(twoOrgs, 'prj_b1')).toMatchObject({ tenant: { administers: false, staff: false } })
  })

  it('reports a project the caller does not reach, and never substitutes another', () => {
    expect(resolveTenant(twoOrgs, 'prj_gone')).toEqual({ state: 'unavailable', projectID: 'prj_gone' })
  })

  it('reports a membership that ended: the project is no longer among what the caller reaches', () => {
    const afterRemoval: Me = { ...twoOrgs, orgs: [twoOrgs.orgs[0]], projects: twoOrgs.projects.filter(p => p.org_id === 'org_a') }
    expect(resolveTenant(afterRemoval, 'prj_b1')).toEqual({ state: 'unavailable', projectID: 'prj_b1' })
  })

  it('reports a caller who reaches nothing', () => {
    expect(resolveTenant({ kind: 'user', orgs: [], projects: [] }, '')).toEqual({ state: 'none' })
  })

  it('lets staff administer an org they do not belong to, and lands them in their own', () => {
    const staff: Me = {
      kind: 'staff',
      user_id: 'u9',
      orgs: [
        { id: 'org_x', name: 'Customer', created_at: at },
        { id: 'org_s', name: 'Staff home', role: 'owner', created_at: at },
      ],
      projects: [
        { id: 'prj_x', org_id: 'org_x', name: 'default', created_at: at },
        { id: 'prj_s', org_id: 'org_s', name: 'default', created_at: at },
      ],
    }
    expect(resolveTenant(staff, '')).toMatchObject({ tenant: { project: { id: 'prj_s' }, staff: true } })
    expect(resolveTenant(staff, 'prj_x')).toMatchObject({ tenant: { org: { id: 'org_x' }, staff: true, administers: true } })
  })

  it('lands a staff token that is no person in the first org there is', () => {
    const token: Me = {
      kind: 'staff',
      orgs: [{ id: 'org_x', name: 'Customer', created_at: at }],
      projects: [{ id: 'prj_x', org_id: 'org_x', name: 'default', created_at: at }],
    }
    expect(resolveTenant(token, '')).toMatchObject({ tenant: { project: { id: 'prj_x' } } })
  })
})
