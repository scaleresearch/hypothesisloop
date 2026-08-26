'use client'

import { useState } from 'react'
import useSWR from 'swr'
import { fetchClusters, fetchResourceCapacity, fetchClusterSettings, putClusterSettings } from '@/lib/api'
import type { ClustersResponse, ClusterInfo } from '@/types'
import { PageHeader } from '@/components/ui/page-header'
import { Pod, PodHeader, PodContent } from '@/components/ui/pod'
import { Button } from '@/components/ui/button'
import { StatTile } from '@/components/ui/stat-tile'
import { Loading, ErrorMessage, EmptyState } from '@/components/ui/status-message'
import { semantic } from '@/lib/colors'

// Inline editor for one cluster's cluster_settings row (scale_up_timeout_seconds,
// max_speculative_accelerators). Only rendered for autoscaler-enabled clusters — a non-autoscaler
// cluster never speculates, so its settings would have nothing to affect.
function ClusterSettingsEditor({ clusterID }: { clusterID: string }) {
  const { data, isLoading, mutate } = useSWR(
    clusterID ? ['cluster-settings', clusterID] : null,
    () => fetchClusterSettings(clusterID),
  )
  const [timeout_, setTimeout_] = useState('')
  const [cap, setCap] = useState('')
  const [saving, setSaving] = useState(false)
  const [editing, setEditing] = useState(false)

  if (!clusterID) return <span className="text-muted" style={{ fontSize: 12 }}>no cluster_id reported</span>
  if (isLoading) return <Loading />

  if (!editing) {
    return (
      <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
        <span className="mono text-muted" style={{ fontSize: 12 }}>
          timeout: {data?.scale_up_timeout_seconds ?? 'default'}s · cap: {data?.max_speculative_accelerators ?? 'none'}
        </span>
        <Button size="sm" onClick={() => {
          setTimeout_(data?.scale_up_timeout_seconds != null ? String(data.scale_up_timeout_seconds) : '')
          setCap(data?.max_speculative_accelerators != null ? String(data.max_speculative_accelerators) : '')
          setEditing(true)
        }}>Edit</Button>
      </div>
    )
  }

  return (
    <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
      <input
        style={{ width: 110, padding: '4px 8px', border: '1px solid var(--border)', borderRadius: 6, fontSize: 12, background: 'var(--surface-2)', color: 'var(--foreground)' }}
        placeholder="timeout s (< 1800)" value={timeout_}
        onChange={e => setTimeout_(e.target.value)}
      />
      <input
        style={{ width: 80, padding: '4px 8px', border: '1px solid var(--border)', borderRadius: 6, fontSize: 12, background: 'var(--surface-2)', color: 'var(--foreground)' }}
        placeholder="max accel" value={cap}
        onChange={e => setCap(e.target.value)}
      />
      <Button size="sm" disabled={saving} onClick={async () => {
        setSaving(true)
        try {
          await putClusterSettings(clusterID, {
            scale_up_timeout_seconds: timeout_ === '' ? null : Number(timeout_),
            max_speculative_accelerators: cap === '' ? null : Number(cap),
          })
          await mutate()
          setEditing(false)
        } finally {
          setSaving(false)
        }
      }}>Save</Button>
      <Button size="sm" onClick={() => setEditing(false)}>Cancel</Button>
    </div>
  )
}

function ConnectedClusters() {
  const { data, error, isLoading, mutate } = useSWR<ClustersResponse>(
    'clusters',
    fetchClusters,
    { refreshInterval: 5_000 },
  )

  const clusterList = data?.clusters ?? []
  const connectedCount = clusterList.filter(c => c.connected).length

  return (
    <>
      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(3, minmax(0, 1fr))', gap: 10, marginBottom: 16 }}>
        <StatTile label="Registered Clusters" value={clusterList.length} />
        <StatTile
          label="Connected"
          value={connectedCount}
          color={clusterList.length > 0 && connectedCount === clusterList.length ? semantic.success : connectedCount > 0 ? semantic.warning : semantic.danger}
        />
        <StatTile
          label="Disconnected"
          value={clusterList.length - connectedCount}
          color={clusterList.length - connectedCount > 0 ? semantic.danger : undefined}
        />
      </div>

      <Pod>
        <PodHeader style={{ justifyContent: 'space-between' }}>
          <span>Connected Clusters</span>
          <Button size="sm" onClick={() => mutate()}>Refresh</Button>
        </PodHeader>
        <PodContent>
          {isLoading && <Loading />}
          {error && <ErrorMessage>Cannot reach scheduler service — is the stack running?</ErrorMessage>}
          {data && data.clusters.length === 0 && (
            <EmptyState>
              No cluster has ever connected. Install a cluster-agent (<code>make cluster-agent-up CLUSTER=&lt;name&gt;</code>)
              pointed at this control plane.
            </EmptyState>
          )}
          {data && data.clusters.length > 0 && (
            <table className="wa-table">
              <thead>
                <tr>
                  <th>Cluster</th>
                  <th>Status</th>
                  <th>Last Seen</th>
                  <th>Autoscaler</th>
                  <th>Speculation Settings</th>
                </tr>
              </thead>
              <tbody>
                {data.clusters.map((c: ClusterInfo) => (
                  <tr key={c.cluster_name}>
                    <td className="mono" style={{ fontWeight: 600 }}>
                      {c.cluster_name}
                      {c.cluster_id && <div className="text-muted" style={{ fontSize: 11 }}>{c.cluster_id}</div>}
                    </td>
                    <td>
                      <span style={{
                        display: 'inline-flex', alignItems: 'center', gap: 6,
                        color: c.connected ? semantic.success : semantic.danger, fontSize: 12, fontWeight: 600,
                      }}>
                        <span style={{
                          width: 7, height: 7, borderRadius: '50%',
                          background: c.connected ? semantic.success : semantic.danger, display: 'inline-block',
                          boxShadow: c.connected ? '0 0 6px rgba(74, 222, 128, 0.6)' : '0 0 6px rgba(242, 89, 107, 0.5)',
                        }} />
                        {c.connected ? 'Connected' : 'Disconnected'}
                      </span>
                    </td>
                    <td className="mono text-muted">{new Date(c.last_seen_at).toLocaleString()}</td>
                    <td>
                      <span style={{ fontSize: 12, fontWeight: 600, color: c.autoscaler_enabled ? semantic.success : undefined }}>
                        {c.autoscaler_enabled ? 'Enabled' : 'Off'}
                      </span>
                    </td>
                    <td>
                      {c.autoscaler_enabled
                        ? <ClusterSettingsEditor clusterID={c.cluster_id} />
                        : <span className="text-muted" style={{ fontSize: 12 }}>n/a</span>}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </PodContent>
      </Pod>
    </>
  )
}

function LiveCapacity() {
  const { data, error, isLoading, mutate } = useSWR(
    'resource-capacity',
    fetchResourceCapacity,
    { refreshInterval: 5_000 },
  )

  const clusters = data?.clusters ?? []
  // Same numbers agents get from GET /resource-catalog/capacity — this table exists so an
  // operator can see at a glance what agents will see before they submit a job.
  const rows = clusters.flatMap(c =>
    c.accelerators.map(a => ({ cluster: c.cluster_name, ...a })),
  )

  return (
    <Pod>
      <PodHeader style={{ justifyContent: 'space-between' }}>
        <span>Capacity</span>
        <Button size="sm" onClick={() => mutate()}>Refresh</Button>
      </PodHeader>
      <PodContent>
        {isLoading && <Loading />}
        {error && <ErrorMessage>Cannot reach quota service — is the stack running?</ErrorMessage>}
        {data && rows.length === 0 && (
          <EmptyState>
            No cluster has reported live accelerator capacity yet — a cluster-agent reports
            allocatable-minus-requested accelerators per type on every desired-state poll; this
            table is empty until at least one poll has landed.
          </EmptyState>
        )}
        {data && rows.length > 0 && (
          <table className="wa-table">
            <thead>
              <tr>
                <th>Cluster</th>
                <th>Accelerator Type</th>
                <th>Available</th>
                <th>Total</th>
              </tr>
            </thead>
            <tbody>
              {rows.map(r => (
                <tr key={`${r.cluster}-${r.accelerator_type}`}>
                  <td className="mono">{r.cluster}</td>
                  <td className="mono" style={{ fontWeight: 600 }}>{r.accelerator_type}</td>
                  <td className="mono" style={{ color: r.available > 0 ? semantic.success : semantic.danger }}>
                    {r.available}
                  </td>
                  <td className="mono text-muted">{r.total}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </PodContent>
    </Pod>
  )
}

export default function ClusterPage() {
  return (
    <div>
      <PageHeader
        title="Compute Resources"
        description="Target Kubernetes clusters registered with this control plane. The control plane
          never connects to a cluster directly — each cluster runs its own cluster-agent,
          which polls here for work and reports its own presence via heartbeat."
      />

      <ConnectedClusters />
      <LiveCapacity />
    </div>
  )
}
