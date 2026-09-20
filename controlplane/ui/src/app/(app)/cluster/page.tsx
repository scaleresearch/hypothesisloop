'use client'

import useSWR from 'swr'
import { fetchAllClusters, fetchResourceCapacity } from '@/lib/api'
import type { ClustersResponse, ClusterInfo } from '@/types'
import { useTenant } from '@/lib/tenant'
import { ClusterSettingsEditor } from '@/components/cluster-settings-editor'
import { OrgClusters } from '@/components/org-clusters'
import { PageHeader } from '@/components/ui/page-header'
import { Pod, PodHeader, PodContent } from '@/components/ui/pod'
import { StatTile } from '@/components/ui/stat-tile'
import { Loading, ErrorMessage, EmptyState } from '@/components/ui/status-message'
import { semantic } from '@/lib/colors'

function AllClustersCounts({ clusters }: { clusters: ClusterInfo[] }) {
  const connected = clusters.filter(c => c.connected).length
  return (
    <div style={{ display: 'grid', gridTemplateColumns: 'repeat(3, minmax(0, 1fr))', gap: 10, marginBottom: 16 }}>
      <StatTile label="Clusters, every org" value={clusters.length} />
      <StatTile
        label="Connected"
        value={connected}
        color={clusters.length > 0 && connected === clusters.length ? semantic.success : connected > 0 ? semantic.warning : semantic.danger}
      />
      <StatTile
        label="Disconnected"
        value={clusters.length - connected}
        color={clusters.length - connected > 0 ? semantic.danger : undefined}
      />
    </div>
  )
}

// Every org's clusters with their live occupancy, which the API answers for staff alone.
function AllClusters() {
  const { data, error, isLoading } = useSWR<ClustersResponse>(
    'all-clusters',
    fetchAllClusters,
    { refreshInterval: 5_000 },
  )

  return (
    <>
      {data && !error && <AllClustersCounts clusters={data.clusters} />}

      <Pod>
        <PodHeader>All Clusters · every org · staff only</PodHeader>
        <PodContent>
          {isLoading && <Loading />}
          {error && <ErrorMessage>Could not load the clusters of every org: {error.message}</ErrorMessage>}
          {data && data.clusters.length === 0 && (
            <EmptyState>
              No cluster has ever connected. Install a cluster-agent (<code>make cluster-agent-up CLUSTER=&lt;name&gt; API_URL=&lt;control-service-url&gt; METRICS_URL=&lt;metrics-service-url&gt;</code>)
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
                  <th>Scheduler settings</th>
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
                      {c.cluster_id
                        ? <ClusterSettingsEditor clusterID={c.cluster_id} />
                        : <span className="text-muted" style={{ fontSize: 12 }}>no cluster_id reported</span>}
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
  const { org } = useTenant()
  const { data, error, isLoading } = useSWR(
    ['resource-capacity', org.id],
    () => fetchResourceCapacity(org.id),
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
      <PodHeader>Capacity {org.name} can use</PodHeader>
      <PodContent>
        {isLoading && <Loading />}
        {error && <ErrorMessage>Could not load live capacity: {error.message}</ErrorMessage>}
        {data && rows.length === 0 && (
          <EmptyState>
            No cluster the jobs of {org.name} can be placed on reports live accelerator capacity
            right now. A cluster-agent reports allocatable-minus-requested accelerators per type
            on every desired-state poll, so this table fills once one of its clusters, or one the
            platform shares, has polled.
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
                <th title="What one accelerator of this type costs for an hour, in accelerator hours">AccH per hour</th>
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
                  <td className="mono">{r.acch_per_hour}</td>
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
  const { org, staff } = useTenant()
  return (
    <div>
      <PageHeader
        title="Compute Resources"
        description="The clusters your jobs can run on. The control plane never connects to a
          cluster directly: each cluster runs its own cluster-agent, which polls here for work
          and reports its own presence via heartbeat."
      />

      <Pod>
        <PodHeader>Clusters {org.name} can run on</PodHeader>
        <PodContent scrollX>
          <OrgClusters manage={false} />
        </PodContent>
      </Pod>
      <LiveCapacity />
      {staff && <AllClusters />}
    </div>
  )
}
