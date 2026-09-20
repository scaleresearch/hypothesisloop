'use client'

import { useState } from 'react'
import { createProject, renameOrg, renameProject } from '@/lib/api'
import { useRefreshMe, useTenant } from '@/lib/tenant'
import { APIKeys } from '@/components/api-keys'
import { OrgClusters } from '@/components/org-clusters'
import { Button } from '@/components/ui/button'
import { Link } from '@/components/ui/link'
import { PageHeader } from '@/components/ui/page-header'
import { Pod, PodHeader, PodContent } from '@/components/ui/pod'

/** A name with a Save beside it. `save` rejects with the API's own reason, which is shown. */
function NameForm({ label, initial, action, placeholder, save }: {
  label: string
  initial: string
  action: string
  placeholder?: string
  save: (name: string) => Promise<unknown>
}) {
  const [name, setName] = useState(initial)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  async function submit(e: React.FormEvent) {
    e.preventDefault()
    setBusy(true)
    setError('')
    try {
      await save(name.trim())
      if (initial === '') setName('')
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <form className="settings-row" onSubmit={submit}>
      <input
        className="field-sm"
        style={{ width: 260 }}
        aria-label={label}
        placeholder={placeholder}
        value={name}
        onChange={e => setName(e.target.value)}
      />
      <Button size="sm" type="submit" disabled={busy || name.trim() === '' || name.trim() === initial}>
        {busy ? 'Saving…' : action}
      </Button>
      {error && <span className="text-error" style={{ fontSize: 12 }}>{error}</span>}
    </form>
  )
}

function Organization() {
  const { org } = useTenant()
  const refreshMe = useRefreshMe()
  return (
    <Pod>
      <PodHeader>Organization</PodHeader>
      <PodContent>
        <NameForm
          // Keyed by the stored name, so a rename made in another tab resets the field.
          key={org.name}
          label="Organization name"
          initial={org.name}
          action="Rename"
          save={async name => { await renameOrg(org.id, name); await refreshMe() }}
        />
        <p className="text-dim mono" style={{ fontSize: 11, marginTop: 8 }}>{org.id}</p>
      </PodContent>
    </Pod>
  )
}

function Projects() {
  const { me, org, project } = useTenant()
  const refreshMe = useRefreshMe()
  return (
    <Pod>
      <PodHeader>Projects</PodHeader>
      <PodContent>
        <div style={{ display: 'grid', gap: 10 }}>
          {me.projects.filter(p => p.org_id === org.id).map(p => (
            <div key={p.id} style={{ display: 'grid', gap: 3 }}>
              <NameForm
                key={p.name}
                label={`Name of project ${p.name}`}
                initial={p.name}
                action="Rename"
                save={async name => { await renameProject(p.id, name); await refreshMe() }}
              />
              <span className="text-dim mono" style={{ fontSize: 11 }}>
                {p.id}{p.id === project.id && ' · selected'}
              </span>
            </div>
          ))}
          <div style={{ borderTop: '1px solid var(--border)', paddingTop: 10 }}>
            <NameForm
              label="New project name"
              initial=""
              action="Create project"
              placeholder="new project name"
              save={async name => { await createProject(org.id, name); await refreshMe() }}
            />
          </div>
        </div>
      </PodContent>
    </Pod>
  )
}

export default function SettingsPage() {
  const { org, project, administers } = useTenant()
  return (
    <div>
      <PageHeader title="Settings" description={`${org.name}: its name, projects, API keys and clusters.`} />

      {!administers && (
        <p className="text-muted" style={{ fontSize: 13, marginBottom: 16 }}>
          You are a member of {org.name}. Only an owner can rename it or manage its projects, API keys and clusters.
        </p>
      )}

      {administers && (
        <>
          <Organization />
          <Projects />
          <Pod>
            <PodHeader>API keys</PodHeader>
            <PodContent scrollX>
              <APIKeys />
            </PodContent>
          </Pod>
        </>
      )}

      <Pod>
        <PodHeader>Clusters</PodHeader>
        <PodContent scrollX>
          <OrgClusters manage />
        </PodContent>
      </Pod>

      <Pod>
        <PodHeader>Repository</PodHeader>
        <PodContent>
          <p className="text-muted" style={{ fontSize: 13 }}>
            The GitHub repository the agents and jobs of {project.name} clone, and its agents push their branches to.{' '}
            <Link href="/settings/repository" className="text-link">Repository of {project.name}</Link>
          </p>
        </PodContent>
      </Pod>
    </div>
  )
}
