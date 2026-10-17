'use client'

import { useState } from 'react'
import { createOrg, createProject } from '@/lib/api/accounts'
import { usePathname, useRouter } from 'next/navigation'
import { sectionOf, sectionPath } from '@/lib/routes'
import { orgLabels, useRefreshMe, useTenant } from '@/lib/tenant'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { InlineError } from '@/components/ui/status-message'
import { Inline, Stack } from '@/components/ui/stack'
import { Eyebrow } from '@/components/ui/text'
import { SearchableSelect } from '@/components/ui/searchable-select'
import type { Project } from '@/types/api'

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
    <Stack as="form" gap="xs" className="wa-nav-create" onSubmit={submit}>
      <Input
        fieldSize="sm"
        autoFocus
        aria-label={`New ${what} name`}
        placeholder={`${what} name`}
        value={name}
        onChange={e => setName(e.target.value)}
      />
      <Inline gap="xs" nowrap>
        <Button size="sm" variant="primary" type="submit" disabled={busy || name.trim() === ''}>
          {busy ? 'Creating…' : 'Create'}
        </Button>
        <Button size="sm" type="button" disabled={busy} onClick={() => { setOpen(false); setError('') }}>Cancel</Button>
      </Inline>
      {error && <InlineError>{error}</InlineError>}
    </Stack>
  )
}

/**
 * Picks the org and the project every page shows, and creates either. The choice lives in the URL.
 * Switching lands on the same section of the other project with its filters cleared: a record
 * open in one project and a filter on its experiments mean nothing in another.
 */
export function ProjectSwitcher() {
  const { me, org, project } = useTenant()
  const editsOrg = org.actions.includes('edit')
  const router = useRouter()
  const section = sectionOf(usePathname()) ?? 'experiments'
  const refreshMe = useRefreshMe()
  const selectProject = (to: Project) => router.push(sectionPath(section, to))

  const projectsOf = (orgID: string) => me.projects.filter(p => p.org_id === orgID)
  const orgs = me.orgs.filter(o => projectsOf(o.id).length > 0)
  const labels = orgLabels(me)

  // What was just created is switched to once GET /me lists it, which is what every page reads.
  async function land(find: (p: Project) => boolean, missing: string) {
    const created = (await refreshMe()).projects.find(find)
    if (!created) throw new Error(missing)
    selectProject(created)
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
    <Stack gap="sm" className="wa-nav-tenant px-3 pt-3">
      <Stack as="label" gap="xs">
        <Eyebrow>Organization</Eyebrow>
        <SearchableSelect
          hideAllOption
          value={org.id}
          options={orgs.map(o => ({ value: o.id, label: labels.get(o.id) as string }))}
          // Clearing the field picks nothing; switching org lands on its first project.
          onChange={id => { if (id && id !== org.id) selectProject(projectsOf(id)[0]) }}
        />
      </Stack>
      <CreateByName what="organization" create={newOrg} />
      <Stack as="label" gap="xs">
        <Eyebrow>Project</Eyebrow>
        <SearchableSelect
          hideAllOption
          value={project.id}
          options={projectsOf(org.id).map(p => ({ value: p.id, label: p.name }))}
          onChange={id => { if (id && id !== project.id) selectProject(projectsOf(org.id).find(p => p.id === id) as Project) }}
        />
      </Stack>
      {editsOrg && <CreateByName what="project" create={newProject} />}
    </Stack>
  )
}
