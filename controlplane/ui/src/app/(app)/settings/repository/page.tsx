'use client'

import { useEffect, useMemo, useRef, useState } from 'react'
import { useSearchParams } from 'next/navigation'
import useSWR from 'swr'
import { ApiError, attachProjectRepo, detachProjectRepo, fetchGitHubApp, fetchProjectRepo } from '@/lib/api'
import { useProjectRouter, useRecordProject } from '@/lib/query-state'
import { REPOSITORY_PATH, attachStep, authorizeAddress, endAttach, installAddress, leaveFor, redirectURI } from '@/lib/repo-attach'
import { useTenant } from '@/lib/tenant'
import type { GitHubApp, ProjectRepo } from '@/types'
import { Button } from '@/components/ui/button'
import { Link } from '@/components/ui/link'
import { PageHeader } from '@/components/ui/page-header'
import { Pod, PodHeader, PodContent } from '@/components/ui/pod'
import { Loading, ErrorMessage } from '@/components/ui/status-message'

function message(e: unknown): string {
  return e instanceof Error ? e.message : String(e)
}

function AppUnavailable({ error }: { error: Error }) {
  if (error instanceof ApiError && error.status === 503) {
    return <ErrorMessage>GitHub is not configured on this deployment, so no repository can be attached.</ErrorMessage>
  }
  return <ErrorMessage>Could not load the platform&apos;s GitHub App: {error.message}</ErrorMessage>
}

function Attached({ repo, onDetached }: { repo: ProjectRepo; onDetached: () => Promise<unknown> }) {
  const { project, administers } = useTenant()
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  async function detach() {
    if (!confirm(`Detach ${repo.repo} from ${project.name}? Agents and jobs of its running platform experiments can no longer clone, fetch or push within the hour.`)) return
    setBusy(true)
    setError('')
    try {
      await detachProjectRepo(project.id)
      await onDetached()
    } catch (e) {
      setError(message(e))
      setBusy(false)
    }
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
        ? <div><Button variant="danger" size="sm" disabled={busy} onClick={detach}>{busy ? 'Detaching…' : 'Detach repository'}</Button></div>
        : <p className="text-muted" style={{ fontSize: 13 }}>Only an owner of the organization can detach it.</p>}
      {error && <ErrorMessage>{error}</ErrorMessage>}
    </div>
  )
}

function Install({ app }: { app: GitHubApp }) {
  const { project } = useTenant()
  return (
    <div style={{ display: 'grid', gap: 12 }}>
      <p className="text-muted" style={{ fontSize: 13 }}>
        No repository is attached to {project.name}. Attaching one takes three steps: install the
        platform&apos;s GitHub App on the repository, confirm on GitHub who you are, and name the
        repository here. You must be able to push to it, since agents will.
      </p>
      <div>
        <Button variant="primary" onClick={() => leaveFor(installAddress(app.install_url, project.id))}>
          Install the GitHub App
        </Button>
      </div>
    </div>
  )
}

function Authorize({ app, installationID }: { app: GitHubApp; installationID: number }) {
  // Leaving is done once: an effect that runs again would ask GitHub for a second code.
  const left = useRef(false)
  useEffect(() => {
    if (left.current) return
    left.current = true
    leaveFor(authorizeAddress(app.authorize_url, installationID))
  }, [app, installationID])
  return <Loading text="Going to GitHub to confirm who you are…" />
}

function Attach({ app, installationID, code, onAttached }: {
  app: GitHubApp
  installationID: number
  code: string
  onAttached: () => Promise<unknown>
}) {
  const { project } = useTenant()
  const [repo, setRepo] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  async function attach(e: React.FormEvent) {
    e.preventDefault()
    setBusy(true)
    setError('')
    try {
      await attachProjectRepo(project.id, { installationID, repo: repo.trim(), code, redirectURI: redirectURI() })
      endAttach()
      await onAttached()
    } catch (e) {
      setError(message(e))
      setBusy(false)
    }
  }

  return (
    <div style={{ display: 'grid', gap: 12 }}>
      <p className="text-muted" style={{ fontSize: 13 }}>
        The App is installed and GitHub confirmed who you are. Name the repository to attach to {project.name}.
      </p>
      <form className="settings-row" onSubmit={attach}>
        <input
          className="field-sm mono"
          style={{ width: 320 }}
          aria-label="Repository"
          placeholder="owner/name"
          value={repo}
          onChange={e => setRepo(e.target.value)}
        />
        <Button size="sm" variant="primary" type="submit" disabled={busy || repo.trim() === ''}>
          {busy ? 'Attaching…' : 'Attach repository'}
        </Button>
      </form>
      {error && (
        <>
          <ErrorMessage>{error}</ErrorMessage>
          <p className="text-muted" style={{ fontSize: 13 }}>
            GitHub&apos;s confirmation works once and for ten minutes. To try again after it was used:{' '}
            <Button variant="link" onClick={() => leaveFor(authorizeAddress(app.authorize_url, installationID))}>
              confirm on GitHub again
            </Button>
          </p>
        </>
      )}
    </div>
  )
}

function Repository() {
  const { org, project, administers } = useTenant()
  const router = useProjectRouter()
  const params = useSearchParams()
  const query = params.toString()
  const at = useMemo(() => attachStep(new URLSearchParams(query)), [query])
  const returning = at.step === 'authorize' || at.step === 'attach' ? at : null
  useRecordProject(returning?.projectID)

  const repo = useSWR(['project-repo', project.id], () => fetchProjectRepo(project.id))
  const app = useSWR(administers ? 'github-app' : null, fetchGitHubApp, { shouldRetryOnError: false })

  if (at.step === 'refused') {
    return (
      <>
        <ErrorMessage>{at.reason}</ErrorMessage>
        <p style={{ fontSize: 13 }}><Link href={REPOSITORY_PATH} className="text-link">Start again</Link></p>
      </>
    )
  }
  // GitHub's return names no project: the URL is being pointed at the one the attempt began in.
  if (returning && returning.projectID !== project.id) return <Loading />
  if (repo.error) return <ErrorMessage>Could not load the repository of {project.name}: {repo.error.message}</ErrorMessage>
  if (repo.data === undefined) return <Loading />

  if (!returning && repo.data) return <Attached repo={repo.data} onDetached={() => repo.mutate()} />
  if (!administers) {
    return (
      <p className="text-muted" style={{ fontSize: 13 }}>
        No repository is attached to {project.name}. Only an owner of {org.name} can attach one.
      </p>
    )
  }
  if (app.error) return <AppUnavailable error={app.error} />
  if (!app.data) return <Loading />
  if (at.step === 'authorize') return <Authorize app={app.data} installationID={at.installationID} />
  if (at.step === 'attach') {
    return (
      <Attach
        app={app.data}
        installationID={at.installationID}
        code={at.code}
        onAttached={async () => {
          await repo.mutate()
          router.replace(REPOSITORY_PATH)
        }}
      />
    )
  }
  return <Install app={app.data} />
}

export default function RepositoryPage() {
  const { org, project } = useTenant()
  return (
    <div>
      <PageHeader
        title="Repository"
        description={`The GitHub repository the agents and jobs of ${org.name} / ${project.name} clone.`}
      />
      <Pod>
        <PodHeader>Repository of {project.name}</PodHeader>
        <PodContent scrollX>
          <Repository />
        </PodContent>
      </Pod>
    </div>
  )
}
