'use client'

import { useState } from 'react'
import useSWR from 'swr'
import { fetchOrgClusters, registerCluster } from '@/lib/api'
import type { RegisteredCluster } from '@/types'
import { useTenant } from '@/lib/tenant'
import { ClusterSettingsEditor } from '@/components/cluster-settings-editor'
import { StatusLabel } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Table } from '@/components/ui/table'
import { Secret } from '@/components/ui/secret'
import { Loading, ErrorMessage, EmptyState } from '@/components/ui/status-message'

function shellQuote(text: string): string {
  return `'${text.replaceAll("'", `'\\''`)}'`
}

// The command `hl cluster add` prints, word for word. It reads the runtime token with
// `hl cluster token` when it runs, so the token is on no page and in no shell history.
function startCommand(orgID: string, cluster: RegisteredCluster): string {
  const name = shellQuote(cluster.name)
  return `RUNTIME_TOKEN="$(hl cluster token ${name} --org ${shellQuote(orgID)})" make cluster-agent-up CLUSTER=${name} API_URL=${shellQuote(cluster.api_url)} METRICS_URL=${shellQuote(cluster.metrics_url)}`
}

/**
 * The clusters the org's jobs can run on: its own, with the scheduler settings an owner reads and
 * changes, and any the platform shares with it. With `manage` an owner also registers a cluster
 * and reads the command that starts its agent.
 */
export function OrgClusters({ manage }: { manage: boolean }) {
  const { org, administers } = useTenant()
  const { data, error, mutate } = useSWR(['org-clusters', org.id], () => fetchOrgClusters(org.id), { refreshInterval: 5_000 })
  const [name, setName] = useState('')
  const [busy, setBusy] = useState(false)
  const [actionError, setActionError] = useState('')
  const [registered, setRegistered] = useState<RegisteredCluster | null>(null)
  const manages = manage && administers

  // Registering a name the org already owns changes nothing and answers the same addresses, so
  // adding a cluster and reading an existing one's command again are the same call.
  async function register(clusterName: string): Promise<boolean> {
    setBusy(true)
    setActionError('')
    try {
      setRegistered(await registerCluster(org.id, clusterName))
      await mutate()
      return true
    } catch (e) {
      setActionError(e instanceof Error ? e.message : String(e))
      return false
    } finally {
      setBusy(false)
    }
  }

  async function add(e: React.FormEvent) {
    e.preventDefault()
    if (await register(name.trim())) setName('')
  }

  return (
    <div className="grid gap-3.5">
      {manages && (
        <form className="flex flex-wrap items-center gap-2" onSubmit={add}>
          <Input
            fieldSize="sm"
            mono
            className="w-60"
            aria-label="Cluster name"
            placeholder="cluster name, e.g. lab-k8s"
            value={name}
            onChange={e => setName(e.target.value)}
          />
          <Button size="sm" variant="primary" type="submit" disabled={busy || name.trim() === ''}>
            {busy ? 'Adding…' : 'Add cluster'}
          </Button>
        </form>
      )}
      {actionError && <ErrorMessage>{actionError}</ErrorMessage>}
      {registered && (
        <Secret
          label={`Start the agent of ${registered.name}`}
          value={startCommand(org.id, registered)}
          note={<>
            Run it from a checkout of the repository, with <span className="font-mono">kubectl</span> pointed
            at the cluster and <span className="font-mono">hl</span> signed in as an owner of {org.name}.
            It reads the cluster&apos;s runtime token with <span className="font-mono">hl cluster token</span>{' '}
            when it runs, so the token is not on this page.{' '}
            <span className="font-mono">Show start command</span> in the cluster&apos;s row brings the
            same command back, as does <span className="font-mono">hl cluster add {registered.name}</span>.
          </>}
          dismiss="Hide"
          onDismiss={() => setRegistered(null)}
        />
      )}

      {error && <ErrorMessage>Could not load the clusters of {org.name}: {error.message}</ErrorMessage>}
      {!data && !error && <Loading />}
      {data && data.length === 0 && (
        <EmptyState>
          No cluster is registered for {org.name}, and the platform shares none with it.
          {manages ? ' Add one above to run jobs on your own hardware.' : ' An owner adds one in Settings.'}
        </EmptyState>
      )}
      {data && data.length > 0 && (
        <Table>
          <thead>
            <tr>
              <th>Cluster</th>
              <th>Status</th>
              <th>Belongs to</th>
              <th>Scheduler settings</th>
              {manages && <th />}
            </tr>
          </thead>
          <tbody>
            {data.map(c => (
              <tr key={c.name}>
                <td className="font-mono text-xs font-semibold">
                  {c.name}
                  {c.cluster_id && <div className="text-2xs font-normal text-muted">{c.cluster_id}</div>}
                </td>
                <td><StatusLabel tone={c.connected ? 'success' : 'danger'}>{c.connected ? 'Connected' : 'Disconnected'}</StatusLabel></td>
                <td className="text-xs">
                  {c.owned ? org.name : 'the platform'}
                  {c.shared && <div className="text-2xs text-muted">shared with every org</div>}
                </td>
                <td>
                  {!c.owned
                    ? <span className="text-xs text-muted">set by the platform</span>
                    : !administers
                      ? <span className="text-xs text-muted">set by an owner of {org.name}</span>
                      : c.cluster_id
                        ? <ClusterSettingsEditor clusterID={c.cluster_id} />
                        : <span className="text-xs text-muted">available once its agent has connected</span>}
                </td>
                {manages && (
                  <td>
                    {c.owned && <Button size="sm" disabled={busy} onClick={() => register(c.name)}>Show start command</Button>}
                  </td>
                )}
              </tr>
            ))}
          </tbody>
        </Table>
      )}
    </div>
  )
}
