'use client'

import { useState } from 'react'
import { createOrg, createProject } from '@/lib/api'
import { useProjectParam } from '@/lib/query-state'
import { useRefreshMe, useTenant } from '@/lib/tenant'
import type { Me, Project } from '@/types'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { InlineError } from '@/components/ui/status-message'
import { Eyebrow } from '@/components/ui/text'
import { SearchableSelect } from '@/components/ui/searchable-select'

// Two orgs may carry the same name, so one that shares its name is told apart by the end of its id.
function orgLabels(me: Me): Map<string, string> {
  const shared = (name: string) => me.orgs.filter(o => o.name === name).length > 1
  return new Map(me.orgs.map(o => [o.id, shared(o.name) ? `${o.name} (${o.id.slice(-6)})` : o.name]))
}

/**
 * A "New ..." link that opens a name field in place. `create` makes the thing and switches to
 * it. A refusal is shown and the field stays open with what was typed.
 */
function CreateByName({ what, create }: { what: string; create: (name: string) => Promise<void> }) {
  const [open, setOpen] = useState(false)
  const [name, setName] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  if (!open) return <Button variant="link" size="sm" className="-mt-1 justify-self-start" onClick={() => setOpen(true)}>+ New {what}</Button>

  async function submit(e: React.FormEvent) {
    e.preventDefault()
    setBusy(true)
    setError('')
    try {
      await create(name.trim())
      setName('')
      setOpen(false)
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <form className="wa-nav-create grid gap-1.5" onSubmit={submit}>
      <Input
        fieldSize="sm"
        autoFocus
        aria-label={`New ${what} name`}
        placeholder={`${what} name`}
        value={name}
        onChange={e => setName(e.target.value)}
      />
      <div className="flex gap-1.5">
        <Button size="sm" variant="primary" type="submit" disabled={busy || name.trim() === ''}>
          {busy ? 'Creating…' : 'Create'}
        </Button>
        <Button size="sm" type="button" disabled={busy} onClick={() => { setOpen(false); setError('') }}>Cancel</Button>
      </div>
      {error && <InlineError>{error}</InlineError>}
    </form>
  )
}

/** Picks the org and the project every page shows, and creates either. The choice lives in the URL. */
export function ProjectSwitcher() {
  const { me, org, project, administers } = useTenant()
  const [, selectProject] = useProjectParam()
  const refreshMe = useRefreshMe()

  const projectsOf = (orgID: string) => me.projects.filter(p => p.org_id === orgID)
  const orgs = me.orgs.filter(o => projectsOf(o.id).length > 0)
  const labels = orgLabels(me)

  // What was just created is switched to once GET /me lists it, which is what every page reads.
  async function land(find: (p: Project) => boolean, missing: string) {
    const created = (await refreshMe()).projects.find(find)
    if (!created) throw new Error(missing)
    selectProject(created.id)
  }

  async function newOrg(name: string) {
    const created = await createOrg(name)
    await land(p => p.org_id === created.id, `org ${created.id} was created, but GET /me lists no project of it`)
  }

  async function newProject(name: string) {
    const created = await createProject(org.id, name)
    await land(p => p.id === created.id, `project ${created.id} was created, but GET /me does not list it`)
  }

  return (
    // wa-nav-tenant, wa-nav-create and the labels around the pickers are what the browser suite finds them by.
    <div className="wa-nav-tenant grid gap-2 px-2.5 pt-3">
      <label className="grid gap-1">
        <Eyebrow>Organization</Eyebrow>
        <SearchableSelect
          hideAllOption
          value={org.id}
          options={orgs.map(o => ({ value: o.id, label: labels.get(o.id) as string }))}
          // Clearing the field picks nothing; switching org lands on its first project.
          onChange={id => { if (id && id !== org.id) selectProject(projectsOf(id)[0].id) }}
        />
      </label>
      <CreateByName what="organization" create={newOrg} />
      <label className="grid gap-1">
        <Eyebrow>Project</Eyebrow>
        <SearchableSelect
          hideAllOption
          value={project.id}
          options={projectsOf(org.id).map(p => ({ value: p.id, label: p.name }))}
          onChange={id => { if (id && id !== project.id) selectProject(id) }}
        />
      </label>
      {administers && <CreateByName what="project" create={newProject} />}
    </div>
  )
}
