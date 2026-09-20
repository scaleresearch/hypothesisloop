'use client'

import { useProjectParam } from '@/lib/query-state'
import { useTenant } from '@/lib/tenant'
import { SearchableSelect } from '@/components/ui/searchable-select'

/** Picks the org and the project every page shows. The choice lives in the URL. */
export function ProjectSwitcher() {
  const { me, org, project } = useTenant()
  const [, selectProject] = useProjectParam()

  const projectsOf = (orgID: string) => me.projects.filter(p => p.org_id === orgID)
  const orgs = me.orgs.filter(o => projectsOf(o.id).length > 0)

  return (
    <div className="wa-nav-tenant">
      <label>
        <span className="uppercase-label">Organization</span>
        <SearchableSelect
          hideAllOption
          value={org.id}
          options={orgs.map(o => ({ value: o.id, label: o.name }))}
          // Clearing the field picks nothing; switching org lands on its first project.
          onChange={id => { if (id && id !== org.id) selectProject(projectsOf(id)[0].id) }}
        />
      </label>
      <label>
        <span className="uppercase-label">Project</span>
        <SearchableSelect
          hideAllOption
          value={project.id}
          options={projectsOf(org.id).map(p => ({ value: p.id, label: p.name }))}
          onChange={id => { if (id && id !== project.id) selectProject(id) }}
        />
      </label>
    </div>
  )
}
