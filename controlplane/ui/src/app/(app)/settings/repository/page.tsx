'use client'

import { useEffect, useState } from 'react'
import { useRouter, useSearchParams } from 'next/navigation'
import useSWR from 'swr'
import { APP_NOT_INSTALLED, attachProjectRepo, detachProjectRepo, fetchGitHubApp, fetchProjectRepo } from '@/lib/api'
import {
  REPOSITORY_PATH, beginAttach, isReturn, leaveFor, redirectURI, resetAttach, takeReturn, useAttach, validRepository,
} from '@/lib/repo-attach'
import type { Attach } from '@/lib/repo-attach'
import { useTenant } from '@/lib/tenant'
import type { GitHubApp, ProjectRepo } from '@/types'
import { Button } from '@/components/ui/button'
import { PageHeader } from '@/components/ui/page-header'
import { Card, CardHeader, CardContent } from '@/components/ui/card'
import { Loading, ErrorMessage } from '@/components/ui/status-message'

function InstallLink({ app }: { app: GitHubApp }) {
  return (
    <a href={app.install_url} target="_blank" rel="noreferrer" className="text-link">
      Install the App on a repository
    </a>
  )
}

function Attached({ repo, onDetached }: { repo: ProjectRepo; onDetached: () => void }) {
  const { project, administers } = useTenant()
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  async function detach() {
    if (!confirm(`Detach ${repo.repo} from ${project.name}? Agents and jobs of its running platform experiments can no longer clone, fetch or push within the hour.`)) return
    setBusy(true)
    setError('')
    try {
      await detachProjectRepo(project.id)
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
      setBusy(false)
      return
    }
    onDetached()
  }

  return (
    <div style={{ display: 'grid', gap: 12 }}>
      <table className="wa-table">
        <tbody>
          <tr><th>Repository</th><td className="mono" style={{ fontWeight: 600 }}>{repo.repo}</td></tr>
          <tr><th>Cloned from</th><td className="mono">{repo.clone_url}</td></tr>
          <tr><th>Attached</th><td className="mono text-muted">{new Date(repo.attached_at).toLocaleString()}</td></tr>
        </tbody>
      </table>
      <p className="text-muted" style={{ fontSize: 13 }}>
        Agents push branches named <span className="mono">hl/&lt;platform experiment&gt;/&lt;agent&gt;</span>.
        Protecting other branches is the repository&apos;s own setting on GitHub.
      </p>
      {administers
        ? (
          <>
            <div><Button variant="danger" size="sm" disabled={busy} onClick={detach}>{busy ? 'Detaching…' : 'Detach repository'}</Button></div>
            <p className="text-muted" style={{ fontSize: 13 }}>To attach another repository, detach this one first.</p>
          </>
        )
        : <p className="text-muted" style={{ fontSize: 13 }}>Only an owner of the organization can detach it.</p>}
      {error && <ErrorMessage>{error}</ErrorMessage>}
    </div>
  )
}

// `failed` is the last attempt for this project, when GitHub or the API refused it: its name is
// kept in the field, and trying again starts over on GitHub, since a code works once.
function Connect({ app, failed }: { app: GitHubApp; failed: Extract<Attach, { state: 'failed' }> | null }) {
  const { project } = useTenant()
  const [repo, setRepo] = useState(failed?.repo ?? '')
  const [invalid, setInvalid] = useState(false)
  const [leaving, setLeaving] = useState(false)

  function connect(e: React.FormEvent) {
    e.preventDefault()
    const name = repo.trim()
    if (!validRepository(name)) {
      setInvalid(true)
      return
    }
    setLeaving(true)
    leaveFor(beginAttach(app.authorize_url, project.id, name))
  }

  return (
    <div style={{ display: 'grid', gap: 12 }}>
      <p className="text-muted" style={{ fontSize: 13 }}>
        No repository is attached to {project.name}. Name one the platform&apos;s GitHub App is installed
        on, then confirm on GitHub who you are. You must be able to push to it, since agents will.
      </p>
      <form className="settings-row" onSubmit={connect}>
        <input
          className="field-sm mono"
          style={{ width: 320 }}
          aria-label="Repository"
          placeholder="owner/name"
          value={repo}
          onChange={e => { setRepo(e.target.value); setInvalid(false) }}
        />
        <Button size="sm" variant="primary" type="submit" disabled={leaving || repo.trim() === ''}>
          {leaving ? 'Going to GitHub…' : 'Connect on GitHub'}
        </Button>
      </form>
      {invalid && <ErrorMessage>The repository must be owner/name, as in its address on GitHub.</ErrorMessage>}
      {failed && !invalid && <ErrorMessage>{failed.message}</ErrorMessage>}
      <p className="text-muted" style={{ fontSize: 13 }}>
        {failed?.reason === APP_NOT_INSTALLED ? 'Install it, then connect again: ' : 'Not installed there yet? '}
        <InstallLink app={app} />
      </p>
    </div>
  )
}

function Repository() {
  const { org, project, administers } = useTenant()
  const router = useRouter()
  const params = useSearchParams()
  const query = params.toString()
  const attach = useAttach()
  const returned = isReturn(params)
  // The App's install page comes back here too, with parameters this page has no use for.
  const stray = returned || params.has('installation_id') || params.has('setup_action')

  const repo = useSWR(['project-repo', project.id], () => fetchProjectRepo(project.id))
  const app = useSWR(administers ? 'github-app' : null, fetchGitHubApp, { shouldRetryOnError: false })

  useEffect(() => {
    if (!stray) return
    const returnedTo = returned
      ? takeReturn(new URLSearchParams(query), (projectID, name, code) => attachProjectRepo(projectID, { repo: name, code, redirectURI: redirectURI() }))
      : null
    router.replace(`${REPOSITORY_PATH}?project=${returnedTo ?? project.id}`)
  }, [stray, returned, query, project.id, router])

  const attached = attach.state === 'attached' && attach.projectID === project.id ? attach.view : null
  const { mutate } = repo
  useEffect(() => {
    if (!attached) return
    void mutate(attached, { revalidate: false })
    resetAttach()
  }, [attached, mutate])

  if (attach.state === 'refused') {
    return (
      <>
        <ErrorMessage>{attach.message}</ErrorMessage>
        <div><Button size="sm" onClick={resetAttach}>Start again</Button></div>
      </>
    )
  }
  if (stray) return <Loading />
  if (attach.state === 'attaching' && attach.projectID === project.id) {
    return <Loading text={`Attaching ${attach.repo} to ${project.name}…`} />
  }
  const detached = () => void mutate(null, { revalidate: false })
  if (attached) return <Attached repo={attached} onDetached={detached} />
  if (repo.error) return <ErrorMessage>Could not load the repository of {project.name}: {repo.error.message}</ErrorMessage>
  if (repo.data === undefined) return <Loading />
  if (repo.data) return <Attached repo={repo.data} onDetached={detached} />
  if (!administers) {
    return (
      <p className="text-muted" style={{ fontSize: 13 }}>
        No repository is attached to {project.name}. Only an owner of {org.name} can attach one.
      </p>
    )
  }
  if (app.error) return <ErrorMessage>No repository can be attached right now: {app.error.message}</ErrorMessage>
  if (!app.data) return <Loading />
  return <Connect app={app.data} failed={attach.state === 'failed' && attach.projectID === project.id ? attach : null} />
}

export default function RepositoryPage() {
  const { org, project } = useTenant()
  return (
    <div>
      <PageHeader
        title="Repository"
        description={`The GitHub repository the agents and jobs of ${org.name} / ${project.name} clone.`}
      />
      <Card>
        <CardHeader>Repository of {project.name}</CardHeader>
        <CardContent scrollX>
          <Repository />
        </CardContent>
      </Card>
    </div>
  )
}
