'use client'

import { createContext, useContext } from 'react'
import { useSWRConfig } from 'swr'
import { fetchMe } from '@/lib/api'
import type { Me, Org, PlatformExperiment, Project } from '@/types'

/** The signed-in caller and the org and project every page is showing. */
export interface Tenant {
  me: Me
  org: Org
  project: Project
  /** Platform operators see every org and the views across all of them. */
  staff: boolean
  /** Whether the caller may change the org: rename it, add projects, keys and clusters. */
  administers: boolean
}

export type TenantResolution =
  | { state: 'selected'; tenant: Tenant }
  // The caller reaches no project at all.
  | { state: 'none' }
  // The URL names a project or org the caller does not reach: removed, or their membership ended.
  | { state: 'unavailable'; what: 'project' | 'organization'; id: string }

// Where a caller lands when the URL names no project: the first project of the first org they
// belong to. Staff are listed every org, their own among them, and one who belongs to none lands
// in the first org there is.
function homeProject(me: Me): Project | undefined {
  const own = me.orgs.filter(o => o.role)
  return (own.length > 0 ? own : me.orgs).map(o => me.projects.find(p => p.org_id === o.id)).find(p => p !== undefined)
}

function selected(me: Me, project: Project): TenantResolution {
  const org = me.orgs.find(o => o.id === project.org_id)
  if (!org) throw new Error(`project ${project.id} belongs to org ${project.org_id}, which GET /me did not list`)
  const staff = me.kind === 'staff'
  return { state: 'selected', tenant: { me, org, project, staff, administers: staff || org.role === 'owner' } }
}

/** Picks the project the URL names, or with none named the caller's home project. */
export function resolveTenant(me: Me, projectID: string): TenantResolution {
  const project = projectID ? me.projects.find(p => p.id === projectID) : homeProject(me)
  if (!project) return projectID ? { state: 'unavailable', what: 'project', id: projectID } : { state: 'none' }
  return selected(me, project)
}

/** Picks the org the URL names, shown with the first of its projects the caller reaches. */
export function resolveOrgTenant(me: Me, orgID: string): TenantResolution {
  const project = me.projects.find(p => p.org_id === orgID)
  if (!project) return { state: 'unavailable', what: 'organization', id: orgID }
  return selected(me, project)
}

/**
 * Picks the project a page of a platform experiment is shown in: the experiment's own when the
 * caller reaches it, and otherwise their home project, from which a shared experiment of another
 * org is read.
 */
export function resolveExperimentTenant(me: Me, pe: Pick<PlatformExperiment, 'project_id'>): TenantResolution {
  return resolveTenant(me, me.projects.some(p => p.id === pe.project_id) ? pe.project_id : '')
}

// Two orgs may carry the same name, so one that shares its name is told apart by the end of its id.
export function orgLabels(me: Me): Map<string, string> {
  const shared = (name: string) => me.orgs.filter(o => o.name === name).length > 1
  return new Map(me.orgs.map(o => [o.id, shared(o.name) ? `${o.name} (${o.id.slice(-6)})` : o.name]))
}

export const TenantContext = createContext<Tenant | null>(null)

export function useTenant(): Tenant {
  const tenant = useContext(TenantContext)
  if (!tenant) throw new Error('useTenant is called outside the signed-in app shell')
  return tenant
}

/**
 * Orgs and projects are read from GET /me, so a change to either is followed by reading it again.
 * The answer replaces what every page holds and is handed back to the caller.
 */
export function useRefreshMe(): () => Promise<Me> {
  const { mutate } = useSWRConfig()
  return async () => {
    const me = await mutate('me', fetchMe(), { revalidate: false })
    if (!me) throw new Error('GET /me answered with nothing')
    return me
  }
}
