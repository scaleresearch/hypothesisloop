'use client'

import { useState } from 'react'
import { createProject, renameOrg, renameProject } from '@/lib/api'
import { GITHUB_CARD } from '@/lib/github-connect'
import { useRefreshMe, useTenant } from '@/lib/tenant'
import { APIKeys } from '@/components/api-keys'
import { OrgGitHub } from '@/components/org-github'
import { OrgMembers } from '@/components/org-members'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { InlineError } from '@/components/ui/status-message'
import { PageHeader } from '@/components/ui/page-header'
import { Card, CardHeader, CardContent } from '@/components/ui/card'
import { TextLink } from '@/components/ui/link'

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
    <form className="flex flex-wrap items-center gap-2" onSubmit={submit}>
      <Input
        fieldSize="sm"
        className="w-64"
        aria-label={label}
        placeholder={placeholder}
        value={name}
        onChange={e => setName(e.target.value)}
      />
      <Button size="sm" type="submit" disabled={busy || name.trim() === '' || name.trim() === initial}>
        {busy ? 'Saving…' : action}
      </Button>
      {error && <InlineError>{error}</InlineError>}
    </form>
  )
}

function Organization() {
  const { org } = useTenant()
  const refreshMe = useRefreshMe()
  return (
    <Card>
      <CardHeader>Organization</CardHeader>
      <CardContent>
        <NameForm
          // Keyed by the stored name, so a rename made in another tab resets the field.
          key={org.name}
          label="Organization name"
          initial={org.name}
          action="Rename"
          save={async name => { await renameOrg(org.id, name); await refreshMe() }}
        />
        <p className="mt-2 font-mono text-2xs text-subtle">{org.id}</p>
      </CardContent>
    </Card>
  )
}

function Projects() {
  const { me, org, project } = useTenant()
  const refreshMe = useRefreshMe()
  return (
    <Card>
      <CardHeader>Projects</CardHeader>
      <CardContent>
        <div className="grid gap-2.5">
          {me.projects.filter(p => p.org_id === org.id).map(p => (
            <div key={p.id} className="grid gap-1">
              <NameForm
                key={p.name}
                label={`Name of project ${p.name}`}
                initial={p.name}
                action="Rename"
                save={async name => { await renameProject(p.id, name); await refreshMe() }}
              />
              <span className="font-mono text-2xs text-subtle">
                {p.id}{p.id === project.id && ' · selected'}
              </span>
            </div>
          ))}
          <div className="border-t pt-2.5">
            <NameForm
              label="New project name"
              initial=""
              action="Create project"
              placeholder="new project name"
              save={async name => { await createProject(org.id, name); await refreshMe() }}
            />
          </div>
        </div>
      </CardContent>
    </Card>
  )
}

export default function SettingsPage() {
  const { org, project, administers } = useTenant()
  return (
    <div>
      <PageHeader title="Settings" description={`${org.name}: its name, projects, members, API keys and GitHub accounts.`} />

      {!administers && (
        <p className="mb-4 text-sm text-muted">
          You are a member of {org.name}. Only an owner can rename it or manage its projects, API keys and GitHub accounts.
        </p>
      )}

      {administers && (
        <>
          <Organization />
          <Projects />
          <Card>
            <CardHeader>API keys</CardHeader>
            <CardContent scrollX>
              <APIKeys />
            </CardContent>
          </Card>
        </>
      )}

      <Card>
        <CardHeader>Members</CardHeader>
        <CardContent scrollX>
          <OrgMembers />
        </CardContent>
      </Card>

      <p className="mb-4 text-sm text-muted">
        The clusters of {org.name} are managed on <TextLink href="/cluster">Compute Resources</TextLink>.
      </p>

      <Card id={GITHUB_CARD} className="scroll-mt-4">
        <CardHeader>GitHub</CardHeader>
        <CardContent scrollX>
          <OrgGitHub />
        </CardContent>
      </Card>
    </div>
  )
}
