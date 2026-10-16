import { apiFetch, apiSend, withQuery } from '@/lib/api/request'
import type { ClusterSettings, ClustersResponse, OrgCluster, RegisteredCluster, ResourceCapacity } from '@/types/clusters'

// Every org's clusters with their live occupancy. Staff only: anyone else is answered 403.
export function fetchAllClusters(): Promise<ClustersResponse> {
  return apiFetch<ClustersResponse>(`/internal/clusters`)
}

// The clusters an org's jobs run on, its own and the ones the platform shares, and whether each
// one's cluster-agent is connected right now (has polled desired-state recently). The control
// plane never dials a cluster itself.
export function fetchOrgClusters(orgID: string): Promise<OrgCluster[]> {
  return apiFetch<{ clusters: OrgCluster[] }>(`/orgs/${orgID}/clusters`).then(r => r.clusters)
}

// Whether the org's jobs are placed on a cluster it can use, its own or a shared one. Turning it
// off leaves the jobs already there to finish.
export async function setClusterEnabled(orgID: string, name: string, enabled: boolean): Promise<void> {
  await apiSend('POST', `/orgs/${orgID}/clusters/${encodeURIComponent(name)}/${enabled ? 'enable' : 'disable'}`)
}

// The order the org's jobs are placed on the clusters it registered, every one of them named once,
// first placed first. The API writes the whole order or none of it, and answers the clusters.
export function setClusterOrder(orgID: string, names: string[]): Promise<OrgCluster[]> {
  return apiSend<{ clusters: OrgCluster[] }>('PUT', `/orgs/${orgID}/clusters/order`, { names }).then(r => r.clusters)
}

// Changes the fields named on a cluster the org registered: its settings, where 0 puts one back
// on the scheduler's default, and shared, which staff alone decide.
export async function updateCluster(orgID: string, name: string, change: ClusterSettings & { shared?: boolean }): Promise<void> {
  await apiSend('PATCH', `/orgs/${orgID}/clusters/${encodeURIComponent(name)}`, change)
}

// Ends a cluster: its jobs go back to the queue and its runtime token is refused from then on.
export async function removeCluster(orgID: string, name: string): Promise<void> {
  await apiSend('DELETE', `/orgs/${orgID}/clusters/${encodeURIComponent(name)}`)
}

// Registering a name the org already owns changes nothing and answers the same. The answer's
// runtime token is left behind here, so no page holds it.
export async function registerCluster(orgID: string, name: string): Promise<RegisteredCluster> {
  const registered = await apiSend<RegisteredCluster>('POST', `/orgs/${orgID}/clusters`, { name })
  return {
    name: registered.name, runtime_token: registered.runtime_token, attach_commands: registered.attach_commands,
    api_url: registered.api_url, metrics_url: registered.metrics_url, registry: registered.registry,
  }
}

// What the jobs of one org can be placed on: its own clusters and the ones the platform shares.
export function fetchResourceCapacity(orgID: string): Promise<ResourceCapacity> {
  return apiFetch<ResourceCapacity>(withQuery('/resource-catalog/capacity', { org_id: orgID }))
}
