import type { Project } from '@/types'

/** The pages of a project, each listing what the project holds. */
export type ProjectSection = 'experiments' | 'hypotheses' | 'jobs' | 'agents' | 'statistics'
/** The pages of an org, which every project of it shares. */
export type OrgSection = 'clusters' | 'settings'
export type Section = ProjectSection | OrgSection

const ORG_SECTIONS: readonly string[] = ['clusters', 'settings'] satisfies OrgSection[]

/** Adds the view state a page reads from its query: filters, tabs, a focus. Empty values are left out. */
export function withQuery(path: string, query: Record<string, string | undefined>): string {
  const params = new URLSearchParams(Object.entries(query).filter((e): e is [string, string] => !!e[1]))
  const qs = params.toString()
  return qs ? `${path}?${qs}` : path
}

export function projectPath(projectID: string, section: ProjectSection): string {
  return `/projects/${projectID}/${section}`
}

export function orgPath(orgID: string, section: OrgSection): string {
  return `/orgs/${orgID}/${section}`
}

export function experimentPath(experimentID: string, section?: 'hypotheses' | 'jobs' | 'agents'): string {
  return section ? `/experiments/${experimentID}/${section}` : `/experiments/${experimentID}`
}

export function jobPath(experimentID: string, jobID: string): string {
  return `/experiments/${experimentID}/jobs/${jobID}`
}

export function hypothesisPath(experimentID: string, hypothesisID: string): string {
  return `/experiments/${experimentID}/hypotheses/${hypothesisID}`
}

/** The page of `section` in a project, or in the org it belongs to. */
export function sectionPath(section: Section, project: Project): string {
  return ORG_SECTIONS.includes(section) ? orgPath(project.org_id, section as OrgSection) : projectPath(project.id, section as ProjectSection)
}

/**
 * The section a path lies in: a page of an experiment, or of a record in it, lies in the section
 * that lists it.
 */
export function sectionOf(pathname: string): Section | undefined {
  const [, root, , section] = pathname.split('/')
  if (root === 'projects' || root === 'orgs') return section as Section | undefined
  if (root === 'experiments') return (section ?? 'experiments') as Section
  return undefined
}
