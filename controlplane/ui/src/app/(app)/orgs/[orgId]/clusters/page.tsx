'use client'

import useSWR from 'swr'
import { fetchAllClusters, fetchResourceCapacity } from '@/lib/api/clusters'
import { useTenant } from '@/lib/tenant'
import { budgetUnit, unitCount, unitRate } from '@/lib/units'
import { OrgClusters } from '@/components/clusters/org-clusters'
import { Page } from '@/components/ui/page'
import { Section } from '@/components/ui/section'
import { StatTile } from '@/components/ui/stat-tile'
import { StatusLabel } from '@/components/ui/badge'
import { EmptyState } from '@/components/ui/empty-state'
import { Stack } from '@/components/ui/stack'
import { Table, num } from '@/components/ui/table'
import { Heading, Text } from '@/components/ui/text'
import { Loading, ErrorMessage } from '@/components/ui/status-message'
import { keys } from '@/lib/keys'
import { POLL } from '@/lib/poll'
import { formatDate } from '@/lib/format'
import type { AllClusters, ClusterInfo } from '@/types/cluster-agent-api'
import type { LiveCapacity as LiveCapacityAnswer } from '@/types/api'

function AllClustersCounts({ clusters }: { clusters: ClusterInfo[] }) {
  const connected = clusters.filter(c => c.connected).length
  return (
    <div className="grid grid-cols-3 gap-3">
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
  const { data, error, isLoading } = useSWR<AllClusters>(
    keys.allClusters(),
    fetchAllClusters,
    { refreshInterval: POLL.live },
  )

  return (
    <>
      {data && !error && <AllClustersCounts clusters={data.clusters} />}

      <Section title="All Clusters" note="every org · staff only">
          {isLoading && <Loading />}
          {error && <ErrorMessage>Could not load the clusters of every org: {error.message}</ErrorMessage>}
          {data && data.clusters.length === 0 && (
            <EmptyState>
              No cluster has ever connected.
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
                </tr>
              </thead>
              <tbody>
                {data.clusters.map((c: ClusterInfo) => (
                  <tr key={c.cluster_name}>
                    <td className="font-mono text-xs font-medium">{c.cluster_name}</td>
                    <td><StatusLabel tone={c.connected ? 'success' : 'danger'}>{c.connected ? 'Connected' : 'Disconnected'}</StatusLabel></td>
                    <td className="font-mono text-xs text-muted">{formatDate(c.last_seen_at)}</td>
                    <td>
                      {c.autoscaler_enabled ? <StatusLabel tone="success">Enabled</StatusLabel> : <Text as="span" variant="note">Off</Text>}
                    </td>
                  </tr>
                ))}
              </tbody>
            </Table>
          )}
      </Section>
    </>
  )
}

const gib = (bytes: number) => Math.round((bytes / 2 ** 30) * 10) / 10

const perHour = (acch: number) => `${Math.round(acch * 100) / 100} ${budgetUnit('accelerator')}/h`

// How much of the platform's shared clusters the org's placed jobs may reserve, and reserve now.
function PlatformComputeLimit({ limit }: { limit: NonNullable<LiveCapacityAnswer['platform_compute_limit']> }) {
  return (
    <div className="grid grid-cols-3 gap-3">
      <StatTile
        label="Platform compute held"
        value={`${perHour(limit.org_held)} of ${perHour(limit.org)}`}
        sub="Reserved by placed jobs on shared clusters"
        tone={limit.org_held >= limit.org ? 'warning' : undefined}
      />
      <StatTile label="Per project" value={perHour(limit.project)} sub="The most one project may hold" />
      {limit.project_held !== undefined && (
        <StatTile label="This project holds" value={perHour(limit.project_held)} tone={limit.project_held >= limit.project ? 'warning' : undefined} />
      )}
    </div>
  )
}

function LiveCapacity() {
  const { org } = useTenant()
  const { data, error, isLoading } = useSWR(
    keys.resourceCapacity(org.id),
    () => fetchResourceCapacity(org.id),
    { refreshInterval: POLL.live },
  )

  // The numbers agents get from GET /resource-catalog/capacity, so an operator sees what an
  // agent sees before it submits a job.
  const clusters = data?.clusters ?? []

  return (
    <Section title={`What ${org.name} can run on right now`}>
        {isLoading && <Loading />}
        {error && <ErrorMessage>Could not load live capacity: {error.message}</ErrorMessage>}
        {data?.platform_compute_limit && <PlatformComputeLimit limit={data.platform_compute_limit} />}
        {data && clusters.length === 0 && (
          <EmptyState>
            No cluster {org.name} can use is reporting what it has free right now.
          </EmptyState>
        )}
        {clusters.map(cluster => (
          <Stack as="section" gap="sm" key={cluster.cluster_name} aria-label={cluster.cluster_name} className="border-t py-3 first:border-t-0 first:pt-0 last:pb-0">
            <Heading level={3} className="font-mono">{cluster.cluster_name}</Heading>
            {!cluster.on_demand && cluster.resources && (
              <Text variant="note">
                {cluster.resources.cpu_cores_available} of {unitCount('cpu', cluster.resources.cpu_cores_total)} free
                {' · '}{gib(cluster.resources.memory_bytes_available)} of {gib(cluster.resources.memory_bytes_total)} GiB memory free
              </Text>
            )}
            {cluster.accelerators.length === 0 && <Text variant="note">Reports nothing a job can ask for right now.</Text>}
            {cluster.accelerators.length > 0 && (
              <Table>
                <thead>
                  <tr>
                    <th title="An accelerator type, or CPU cores">Type</th>
                    {cluster.on_demand
                      ? <th title="No machine stands by. The cluster rents one when a job needs it, up to this many at once.">Rented on demand</th>
                      : <th>Free / total</th>}
                    <th className="text-right" title={`What one accelerator of the type, or one CPU core, takes from a budget for an hour of run, in ${budgetUnit('accelerator', 'long')}`}>Budget per hour</th>
                  </tr>
                </thead>
                <tbody>
                  {cluster.accelerators.map(a => (
                    <tr key={a.accelerator_type}>
                      <td className="font-mono text-xs font-medium">{a.accelerator_type}</td>
                      {cluster.on_demand ? (
                        <td className="text-xs">up to {unitCount(a.kind, a.total)}, {a.total - a.available} rented now</td>
                      ) : (
                        <td className="font-mono text-xs">
                          <span className={a.available > 0 ? 'text-success' : 'text-danger'}>{a.available}</span>
                          <span className="text-muted"> / {unitCount(a.kind, a.total)}</span>
                        </td>
                      )}
                      <td className={num}>{unitRate(a.kind, a.acch_per_hour ?? 0)}</td>
                    </tr>
                  ))}
                </tbody>
              </Table>
            )}
          </Stack>
        ))}
    </Section>
  )
}

export default function ClusterPage() {
  const { org, staff } = useTenant()
  return (
    <Page
        title="Compute Resources"
    >
      <Section title={`Where the jobs of ${org.name} run`} scrollX>
        <OrgClusters />
      </Section>
      <LiveCapacity />
      {staff && <AllClusters />}
    </Page>
  )
}
