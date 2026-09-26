'use client'

import useSWR from 'swr'
import { clsx } from 'clsx'
import { fetchAllClusters, fetchResourceCapacity } from '@/lib/api'
import type { ClustersResponse, ClusterInfo } from '@/types'
import { useTenant } from '@/lib/tenant'
import { budgetUnit, unitCount, unitRate } from '@/lib/units'
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

  // The numbers agents get from GET /resource-catalog/capacity, so an operator sees what an
  // agent sees before it submits a job.
  const clusters = data?.clusters ?? []

  return (
    <Card>
      <CardHeader>What {org.name} can run on right now</CardHeader>
      <CardContent>
        {isLoading && <Loading />}
        {error && <ErrorMessage>Could not load live capacity: {error.message}</ErrorMessage>}
        {data && clusters.length === 0 && (
          <EmptyState>
            No cluster {org.name} can use is reporting what it has free right now. This fills in
            once one of its clusters, or one the platform shares, is connected.
          </EmptyState>
        )}
        {clusters.map(cluster => (
          <section key={cluster.cluster_name} aria-label={cluster.cluster_name} className="border-t py-3 first:border-t-0 first:pt-0 last:pb-0">
            <h3 className="mb-1.5 font-mono text-sm font-semibold">{cluster.cluster_name}</h3>
            {cluster.accelerators.length === 0 && <p className="text-xs text-muted">Reports nothing a job can ask for right now.</p>}
            {cluster.accelerators.length > 0 && (
              <Table>
                <thead>
                  <tr>
                    <th title="An accelerator type, or CPU cores">Type</th>
                    <th title="How many are free and how many there are. A cluster that rents machines when a job needs them reports how many it may still rent and the most it rents at once.">Free / total</th>
                    <th title={`What one accelerator of the type, or one CPU core, takes from a budget for an hour of run, in ${budgetUnit('accelerator', 'long')}`}>Budget per hour</th>
                  </tr>
                </thead>
                <tbody>
                  {cluster.accelerators.map(a => (
                    <tr key={a.accelerator_type}>
                      <td className="font-mono text-xs font-semibold">{a.accelerator_type}</td>
                      <td className="font-mono text-xs">
                        <span className={a.available > 0 ? 'text-success' : 'text-danger'}>{a.available}</span>
                        <span className="text-muted"> / {unitCount(a.kind, a.total)}</span>
                      </td>
                      <td className="font-mono text-xs">{unitRate(a.kind, a.acch_per_hour)}</td>
                    </tr>
                  ))}
                </tbody>
              </Table>
            )}
          </section>
        ))}
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
