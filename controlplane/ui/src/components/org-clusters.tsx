'use client'

import { useState } from 'react'
import useSWR from 'swr'
import { fetchOrgClusters, registerCluster } from '@/lib/api'
import type { RegisteredCluster } from '@/types'
import { semantic } from '@/lib/colors'
import { useTenant } from '@/lib/tenant'
import { ClusterSettingsEditor } from '@/components/cluster-settings-editor'
import { Button } from '@/components/ui/button'
import { Secret } from '@/components/ui/secret'
import { Loading, ErrorMessage, EmptyState } from '@/components/ui/status-message'

// The command `hl cluster add` prints, with the addresses the platform says its agents use.
function startCommand(cluster: RegisteredCluster): string {
  return `RUNTIME_TOKEN=${cluster.runtime_token} make cluster-agent-up CLUSTER=${cluster.name} API_URL=${cluster.api_url} METRICS_URL=${cluster.metrics_url}`
}

function Status({ connected }: { connected: boolean }) {
  const color = connected ? semantic.success : semantic.danger
  return (
    <span style={{ display: 'inline-flex', alignItems: 'center', gap: 6, color, fontSize: 12, fontWeight: 600 }}>
      <span style={{ width: 7, height: 7, borderRadius: '50%', background: color }} />
      {connected ? 'Connected' : 'Disconnected'}
    </span>
  )
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

  // Registering a name the org already owns changes nothing and answers with the same token,
  // so adding a cluster and reading an existing one's command again are the same call.
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
    <div style={{ display: 'grid', gap: 14 }}>
      {manages && (
        <form className="settings-row" onSubmit={add}>
          <input
            className="field-sm mono"
            style={{ width: 240 }}
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
          value={startCommand(registered)}
          note={<>
            Run it from a checkout of the repository on a machine that reaches the cluster. The
            command carries the cluster&apos;s runtime token, which is derived from its name:{' '}
            <span className="mono">Show start command</span> in the cluster&apos;s row brings the
            same one back, as does <span className="mono">hl cluster add {registered.name}</span>.
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
        <table className="wa-table">
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
                <td className="mono" style={{ fontWeight: 600 }}>
                  {c.name}
                  {c.cluster_id && <div className="text-muted" style={{ fontSize: 11 }}>{c.cluster_id}</div>}
                </td>
                <td><Status connected={c.connected} /></td>
                <td style={{ fontSize: 12 }}>
                  {c.owned ? org.name : 'the platform'}
                  {c.shared && <div className="text-muted" style={{ fontSize: 11 }}>shared with every org</div>}
                </td>
                <td>
                  {!c.owned
                    ? <span className="text-muted" style={{ fontSize: 12 }}>set by the platform</span>
                    : !administers
                      ? <span className="text-muted" style={{ fontSize: 12 }}>set by an owner of {org.name}</span>
                      : c.cluster_id
                        ? <ClusterSettingsEditor clusterID={c.cluster_id} />
                        : <span className="text-muted" style={{ fontSize: 12 }}>available once its agent has connected</span>}
                </td>
                {manages && (
                  <td>
                    {c.owned && <Button size="sm" disabled={busy} onClick={() => register(c.name)}>Show start command</Button>}
                  </td>
                )}
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  )
}
