'use client'

import useSWR from 'swr'
import { fetchClusters } from '@/lib/api'
import type { ClustersResponse } from '@/types'
import { PageHeader } from '@/components/ui/page-header'
import { Pod, PodHeader, PodContent } from '@/components/ui/pod'
import { Button } from '@/components/ui/button'
import { StatTile } from '@/components/ui/stat-tile'
import { Loading, ErrorMessage, EmptyState } from '@/components/ui/status-message'
import { semantic } from '@/lib/colors'

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
                </tr>
              </thead>
              <tbody>
                {data.clusters.map(c => (
                  <tr key={c.cluster_name}>
                    <td className="mono" style={{ fontWeight: 600 }}>{c.cluster_name}</td>
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

      <Pod>
        <PodHeader>Capacity</PodHeader>
        <PodContent>
          <EmptyState>
            CPU-core capacity is now live: each cluster-agent reports its real
            allocatable-minus-requested cores on every desired-state poll, summed across
            reachable clusters for admission. GPU-flavor capacity still uses static config
            (<code>controlplane/settings/openresearch.yaml</code>) — no GPU cluster is wired up
            yet to report from.
          </EmptyState>
        </PodContent>
      </Pod>
    </div>
  )
}
