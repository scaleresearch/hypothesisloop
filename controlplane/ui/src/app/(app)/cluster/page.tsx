'use client'

import useSWR from 'swr'
import { clsx } from 'clsx'
import { fetchAllClusters, fetchResourceCapacity } from '@/lib/api'
import type { ClustersResponse, ClusterInfo } from '@/types'
import { useTenant } from '@/lib/tenant'
import { unitCount, unitRate } from '@/lib/units'
import { ClusterSettingsEditor } from '@/components/cluster-settings-editor'
import { OrgClusters } from '@/components/org-clusters'
import { PageHeader } from '@/components/ui/page-header'
import { Card, CardHeader, CardContent } from '@/components/ui/card'
import { StatTile } from '@/components/ui/stat-tile'
import { StatusLabel } from '@/components/ui/badge'
import { Table } from '@/components/ui/table'
import { Loading, ErrorMessage, EmptyState } from '@/components/ui/status-message'

function AllClustersCounts({ clusters }: { clusters: ClusterInfo[] }) {
  const connected = clusters.filter(c => c.connected).length
  return (
    <div className="mb-4 grid grid-cols-3 gap-2.5">
      <StatTile label="Clusters, every org" value={clusters.length} />
      <StatTile
        label="Connected"
        value={connected}
        tone={clusters.length > 0 && connected === clusters.length ? 'success' : connected > 0 ? 'warning' : 'danger'}
      />
      <StatTile
        label="Disconnected"
        value={clusters.length - connected}
        tone={clusters.length - connected > 0 ? 'danger' : undefined}
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

      <Card>
        <CardHeader>All Clusters · every org · staff only</CardHeader>
        <CardContent>
          {isLoading && <Loading />}
          {error && <ErrorMessage>Could not load the clusters of every org: {error.message}</ErrorMessage>}
          {data && data.clusters.length === 0 && (
            <EmptyState>
              No cluster has ever connected. Install a cluster-agent (<code className="font-mono text-xs">make cluster-agent-up CLUSTER=&lt;name&gt; API_URL=&lt;control-service-url&gt; METRICS_URL=&lt;metrics-service-url&gt;</code>)
              pointed at this control plane.
            </EmptyState>
          )}
          {data && data.clusters.length > 0 && (
            <Table>
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
                    <td className="font-mono text-xs font-semibold">
                      {c.cluster_name}
                      {c.cluster_id && <div className="text-2xs font-normal text-muted">{c.cluster_id}</div>}
                    </td>
                    <td><StatusLabel tone={c.connected ? 'success' : 'danger'}>{c.connected ? 'Connected' : 'Disconnected'}</StatusLabel></td>
                    <td className="font-mono text-xs text-muted">{new Date(c.last_seen_at).toLocaleString()}</td>
                    <td>
                      <span className={clsx('text-xs font-semibold', c.autoscaler_enabled && 'text-success')}>
                        {c.autoscaler_enabled ? 'Enabled' : 'Off'}
                      </span>
                    </td>
                    <td>
                      {c.cluster_id
                        ? <ClusterSettingsEditor clusterID={c.cluster_id} />
                        : <span className="text-xs text-muted">no cluster_id reported</span>}
                    </td>
                  </tr>
                ))}
              </tbody>
            </Table>
          )}
        </CardContent>
      </Card>
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
    <Card>
      <CardHeader>Capacity {org.name} can use</CardHeader>
      <CardContent>
        {isLoading && <Loading />}
        {error && <ErrorMessage>Could not load live capacity: {error.message}</ErrorMessage>}
        {data && rows.length === 0 && (
          <EmptyState>
            No cluster the jobs of {org.name} can be placed on reports live accelerator or CPU
            class capacity right now. A cluster-agent reports allocatable-minus-requested units per type
            on every desired-state poll, so this table fills once one of its clusters, or one the
            platform shares, has polled.
          </EmptyState>
        )}
        {data && rows.length > 0 && (
          <Table>
            <thead>
              <tr>
                <th>Cluster</th>
                <th title="An accelerator type, or a CPU class whose unit is one core">Type</th>
                <th>Available</th>
                <th>Total</th>
                <th title="What one accelerator of this type, or one core of a CPU class, costs for an hour, in accelerator hours">AccH per hour</th>
              </tr>
            </thead>
            <tbody>
              {rows.map(r => (
                <tr key={`${r.cluster}-${r.accelerator_type}`}>
                  <td className="font-mono text-xs">{r.cluster}</td>
                  <td className="font-mono text-xs font-semibold">{r.accelerator_type}</td>
                  <td className={clsx('font-mono text-xs', r.available > 0 ? 'text-success' : 'text-danger')}>
                    {unitCount(r.kind, r.available)}
                  </td>
                  <td className="font-mono text-xs text-muted">{unitCount(r.kind, r.total)}</td>
                  <td className="font-mono text-xs">{unitRate(r.kind, r.acch_per_hour)}</td>
                </tr>
              ))}
            </tbody>
          </Table>
        )}
      </CardContent>
    </Card>
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

      <Card>
        <CardHeader>Clusters {org.name} can run on</CardHeader>
        <CardContent scrollX>
          <OrgClusters manage={false} />
        </CardContent>
      </Card>
      <LiveCapacity />
      {staff && <AllClusters />}
    </div>
  )
}
