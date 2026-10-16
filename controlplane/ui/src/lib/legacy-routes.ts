import { experimentPath, hypothesisPath, jobPath, projectPath, withQuery } from '@/lib/routes'
import type { ProjectSection, Section } from '@/lib/routes'

/** Where a page moved: a path, or a record whose experiment must be read to know it. */
export type Moved = { path: string } | { record: 'job' | 'hypothesis'; id: string }

// A section's bare name, as the nav calls it, is answered too: it means the caller's own project.
const SECTIONS: Record<string, Section> = {
  'platform-experiments': 'experiments',
  experiments: 'experiments',
  hypotheses: 'hypotheses',
  jobs: 'jobs',
  agents: 'agents',
  statistics: 'statistics',
  cluster: 'clusters',
  clusters: 'clusters',
  settings: 'settings',
}

const PROJECT_SECTIONS: readonly string[] = ['experiments', 'hypotheses', 'jobs', 'agents', 'statistics'] satisfies ProjectSection[]

/** Where `/console?to=` sends a section, for a person whose project only the signed-in shell knows. */
export const CONSOLE_SECTION_PARAM = 'to'

export function isSection(value: string | null): value is Section {
  return value !== null && Object.values(SECTIONS).includes(value as Section)
}

function rest(query: URLSearchParams, without: string[]): Record<string, string> {
  return Object.fromEntries(Array.from(query.entries()).filter(([k]) => !without.includes(k)))
}

// The pages before every page got a path naming what it shows (ca1f2c15). Their links live on in
// bookmarks and shared messages, so each is answered with where it went, for good.
export function movedTo(pathname: string, query: URLSearchParams): Moved | null {
  const [, root, id, extra] = pathname.split('/')
  const section = SECTIONS[root]
  if (!section || extra !== undefined) return null
  if (id) {
    if (root === 'platform-experiments') return { path: withQuery(experimentPath(id), rest(query, [])) }
    if (root === 'hypotheses') return { record: 'hypothesis', id }
    if (root === 'jobs') return { record: 'job', id }
    return null
  }
  const pe = root === 'hypotheses' ? query.get('pe') : root === 'jobs' || root === 'agents' ? query.get('platform_experiment_id') : null
  if (pe) return { path: withQuery(experimentPath(pe, section as 'hypotheses' | 'jobs' | 'agents'), rest(query, ['pe', 'platform_experiment_id', 'project'])) }
  const project = query.get('project')
  if (project && PROJECT_SECTIONS.includes(section)) return { path: withQuery(projectPath(project, section as ProjectSection), rest(query, ['project'])) }
  return { path: withQuery('/console', { [CONSOLE_SECTION_PARAM]: section, ...rest(query, ['project']) }) }
}

/** The new path of a job or hypothesis, from the experiment it belongs to. */
export function recordPath(record: 'job' | 'hypothesis', id: string, experimentID: string): string {
  return record === 'job' ? jobPath(experimentID, id) : hypothesisPath(experimentID, id)
}
