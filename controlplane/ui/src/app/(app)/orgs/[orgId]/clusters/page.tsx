'use client'

import useSWR from 'swr'
import { fetchAllClusters, fetchOrgClusters, fetchResourceCapacity } from '@/lib/api/clusters'
import { clusterLabel } from '@/lib/clusters'
import { useTenant } from '@/lib/tenant'
import { budgetUnit, unitCount, unitRate } from '@/lib/units'
import { OrgClusters } from '@/components/clusters/org-clusters'
import { InfoTip } from '@/components/ui/info-tip'
import { Page } from '@/components/ui/page'
import { Section } from '@/components/ui/section'
import { StatTile } from '@/components/ui/stat-tile'
import { StatusLabel } from '@/components/ui/badge'
import { EmptyState } from '@/components/ui/empty-state'
import { Stack } from '@/components/ui/stack'
import { Table } from '@/components/ui/table'
import { Heading, Text } from '@/components/ui/text'
import { Loading, ErrorMessage } from '@/components/ui/status-message'
import { keys } from '@/lib/keys'
import { POLL } from '@/lib/poll'
import { formatDate, resourceLabel } from '@/lib/format'
import type { AllClusters, ClusterInfo } from '@/types/cluster-agent-api'
import type { LiveCapacity as LiveCapacityAnswer } from '@/types/api'

function AllClustersCounts({ clusters }: { clusters: ClusterInfo[] }) {
  const connected = clusters.filter(c => c.connected).length
  return (
    <div className="grid grid-cols-3 gap-3">
      <StatTile label="Clusters" value={clusters.length} />
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

      <Section title="All clusters" note="every org · staff only">
          {isLoading && <Loading />}
          {error && <ErrorMessage>Could not load clusters: {error.message}</ErrorMessage>}
          {data && data.clusters.length === 0 && (
            <EmptyState>
              No clusters have connected yet.
            </EmptyState>
          )}
          {data && data.clusters.length > 0 && (
            <Table>
              <thead>
                <tr>
                  <th>Cluster</th>
                  <th>Status</th>
                  <th>Last seen</th>
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

const gib = (bytes: number) => round(bytes / 2 ** 30, 1)

const round = (n: number, places: number) => Math.round(n * 10 ** places) / 10 ** places

// How much of the platform's clusters the org's placed jobs may reserve, and reserve now.
function PlatformComputeLimit({ limit, reference }: { limit: NonNullable<LiveCapacityAnswer['platform_compute_limit']>; reference: string | undefined }) {
  const sub = `${budgetUnit('accelerator', 'long', reference)} per hour`
  return (
    <div className="grid grid-cols-3 gap-3">
      <StatTile
        label="Platform usage"
        value={`${round(limit.org_held, 2)} of ${round(limit.org, 2)}`}
        sub={sub}
        tone={limit.org_held >= limit.org ? 'warning' : undefined}
      />
      <StatTile label="Per-project limit" value={round(limit.project, 2)} sub={sub} />
      {limit.project_held !== undefined && (
        <StatTile label="This project" value={round(limit.project_held, 2)} sub={sub} tone={limit.project_held >= limit.project ? 'warning' : undefined} />
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

  // The same list the Clusters section reads, for the vendor of each cluster the platform rents on.
  const { data: listed } = useSWR(keys.orgClusters(org.id), () => fetchOrgClusters(org.id), { refreshInterval: POLL.live })
  const providers = new Map((listed ?? []).map(c => [c.name, c.provider]))

  // The numbers agents get from GET /resource-catalog/capacity, so an operator sees what an
  // agent sees before it submits a job.
  const clusters = data?.clusters ?? []
  const reference = data?.budget_reference || undefined

  return (
    <Section title="Available capacity">
        {isLoading && <Loading />}
        {error && <ErrorMessage>Could not load live capacity: {error.message}</ErrorMessage>}
        {data && clusters.length === 0 && (
          <EmptyState>
            No cluster is reporting capacity.
          </EmptyState>
        )}
        <Stack gap="xl">
        {data?.platform_compute_limit && <PlatformComputeLimit limit={data.platform_compute_limit} reference={reference} />}
        {clusters.map(cluster => (
          <Stack as="section" gap="sm" key={cluster.cluster_name} aria-label={cluster.cluster_name} className="border-t pt-5 first:border-t-0 first:pt-0">
            {providers.get(cluster.cluster_name)
              ? (
                  <Heading level={3}>
                    {clusterLabel({ name: cluster.cluster_name, provider: providers.get(cluster.cluster_name) })}
                    <Text as="span" variant="meta" className="ml-2 font-mono">{cluster.cluster_name}</Text>
                  </Heading>
                )
              : <Heading level={3} className="font-mono">{cluster.cluster_name}</Heading>}
            <Text variant="note">
              {cluster.on_demand ? 'On demand' : `${cluster.hosts} ${cluster.hosts === 1 ? 'node' : 'nodes'}`}
              {' · '}{cluster.multi_node ? 'multi-node jobs' : 'single-node jobs only'}
              <InfoTip>A multi-node job runs only on a multi-node cluster. With topology.spread_across_hosts, the cluster needs at least as many nodes as the job.</InfoTip>
            </Text>
            {!cluster.on_demand && cluster.resources && (
              <Text variant="note">
                {round(cluster.resources.cpu_cores_available, 1)} of {unitCount('cpu', cluster.resources.cpu_cores_total)} free
                {' · '}{gib(cluster.resources.memory_bytes_available)} of {gib(cluster.resources.memory_bytes_total)} GiB memory free
              </Text>
            )}
            {cluster.accelerators.length === 0 && <Text variant="note">No resources available.</Text>}
            {cluster.accelerators.length > 0 && (
              <Table className="mt-1 max-w-2xl">
                <thead>
                  <tr>
                    <th title="An accelerator type, or CPU cores">Type</th>
                    {cluster.on_demand
                      ? <th title="Rented when a job needs it, up to this many at once">On demand</th>
                      : <th>Free / total</th>}
                    <th className="text-right" title={`Budget per accelerator or CPU core per hour, in ${budgetUnit('accelerator', 'long', reference)}`}>Budget per hour</th>
                  </tr>
                </thead>
                <tbody>
                  {cluster.accelerators.map(a => (
                    <tr key={a.accelerator_type}>
                      <td className="font-medium" title={a.accelerator_type}>{resourceLabel(a.accelerator_type)}</td>
                      {cluster.on_demand ? (
                        // available counts only what a market read for waiting jobs found, so it says nothing of what is rented.
                        <td className="tabular-nums">up to {unitCount(a.kind, a.total)}</td>
                      ) : (
                        <td className="tabular-nums">
                          <span className={a.available > 0 ? 'text-success' : 'text-danger'}>{a.available}</span>
                          <span className="text-muted"> / {unitCount(a.kind, a.total)}</span>
                        </td>
                      )}
                      <td className="text-right tabular-nums">{unitRate(a.kind, a.acch_per_hour ?? 0, reference)}</td>
                    </tr>
                  ))}
                </tbody>
              </Table>
            )}
          </Stack>
        ))}
        </Stack>
    </Section>
  )
}

export default function ClusterPage() {
  const { staff } = useTenant()
  return (
    <Page title="Compute">
      <Section title="Clusters" scrollX>
        <OrgClusters />
      </Section>
      <LiveCapacity />
      {staff && <AllClusters />}
    </Page>
  )
}
