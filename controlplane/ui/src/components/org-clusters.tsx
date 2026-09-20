'use client'

import { useState } from 'react'
import useSWR from 'swr'
import { API_URL, fetchOrgClusters, registerCluster } from '@/lib/api'
import type { RegisteredCluster } from '@/types'
import { semantic } from '@/lib/colors'
import { useTenant } from '@/lib/tenant'
import { ClusterSettingsEditor } from '@/components/cluster-settings-editor'
import { Button } from '@/components/ui/button'
import { SecretOnce } from '@/components/ui/secret-once'
import { Loading, ErrorMessage, EmptyState } from '@/components/ui/status-message'

function useOrgClusters() {
  const { org } = useTenant()
  return useSWR(['org-clusters', org.id], () => fetchOrgClusters(org.id), { refreshInterval: 5_000 })
}

/** The org's own clusters: whether each agent is connected, and each cluster's scheduler settings. */
export function OrgClustersTable() {
  const { org } = useTenant()
  const { data, error, isLoading } = useOrgClusters()

  if (isLoading) return <Loading />
  if (error || !data) return <ErrorMessage>Could not load the clusters of {org.name}: {error?.message}</ErrorMessage>
  if (data.length === 0) {
    return (
      <EmptyState>
        {org.name} has no cluster of its own. Its jobs run on the clusters the platform shares. Add
        a cluster in Settings to run them on your own hardware.
      </EmptyState>
    )
  }
  return (
    <table className="wa-table">
      <thead>
        <tr>
          <th>Cluster</th>
          <th>Status</th>
          <th>Takes jobs of</th>
          <th>Scheduler settings</th>
        </tr>
      </thead>
      <tbody>
        {data.map(c => (
          <tr key={c.name}>
            <td className="mono" style={{ fontWeight: 600 }}>
              {c.name}
              {c.cluster_id && <div className="text-muted" style={{ fontSize: 11 }}>{c.cluster_id}</div>}
            </td>
            <td>
              <span style={{
                display: 'inline-flex', alignItems: 'center', gap: 6,
                color: c.connected ? semantic.success : semantic.danger, fontSize: 12, fontWeight: 600,
              }}>
                <span style={{ width: 7, height: 7, borderRadius: '50%', background: c.connected ? semantic.success : semantic.danger }} />
                {c.connected ? 'Connected' : 'Disconnected'}
              </span>
            </td>
            <td style={{ fontSize: 12 }}>{c.shared ? 'every org (shared by the platform)' : 'this org only'}</td>
            <td>
              {c.cluster_id
                ? <ClusterSettingsEditor clusterID={c.cluster_id} />
                : <span className="text-muted" style={{ fontSize: 12 }}>available once its agent has connected</span>}
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  )
}

// The command `hl cluster add` prints. Where the cluster reaches the two services is the
// operator's knowledge: the API address is the one this page uses, and the metrics address is
// left to fill in.
function startCommand(cluster: RegisteredCluster): string {
  return `make cluster-agent-up CLUSTER=${cluster.name} API_URL=${API_URL} METRICS_URL=<metrics-service-url> RUNTIME_TOKEN=${cluster.runtime_token}`
}

/** Registers a cluster name for the org and shows the command that starts its agent. */
export function AddCluster() {
  const { org } = useTenant()
  const { mutate } = useOrgClusters()
  const [name, setName] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [registered, setRegistered] = useState<RegisteredCluster | null>(null)

  async function submit(e: React.FormEvent) {
    e.preventDefault()
    setBusy(true)
    setError('')
    try {
      setRegistered(await registerCluster(org.id, name.trim()))
      setName('')
      await mutate()
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div style={{ display: 'grid', gap: 10 }}>
      <form className="settings-row" onSubmit={submit}>
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
        {error && <span className="text-error" style={{ fontSize: 12 }}>{error}</span>}
      </form>
      {registered && (
        <SecretOnce
          label={`Start the agent of ${registered.name}`}
          value={startCommand(registered)}
          note={<>
            Run it from a checkout of the repository on a machine that reaches the cluster, with
            API_URL and METRICS_URL set to the addresses the cluster reaches the control plane at.
            The command carries the cluster&apos;s runtime token, which this page shows now and
            never again. Adding the same name again returns the same token.
          </>}
          onDismiss={() => setRegistered(null)}
        />
      )}
    </div>
  )
}
