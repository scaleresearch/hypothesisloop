'use client'

import { useState } from 'react'
import useSWR from 'swr'
import { fetchOrgClusters, registerCluster, setClusterEnabled } from '@/lib/api'
import type { OrgCluster, RegisteredCluster } from '@/types'
import { useTenant } from '@/lib/tenant'
import { ClusterSettingsEditor } from '@/components/cluster-settings-editor'
import { Badge, StatusLabel } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Table } from '@/components/ui/table'
import { Secret } from '@/components/ui/secret'
import { Switch } from '@/components/ui/switch'
import { Segmented } from '@/components/ui/tabs'
import { Loading, ErrorMessage, EmptyState } from '@/components/ui/status-message'

function shellQuote(text: string): string {
  return `'${text.replaceAll("'", `'\\''`)}'`
}

// What runs the jobs. The API records no kind: a cluster is a name, and its kind is whichever agent
// connects under it. So the choice here only selects the command that starts that agent.
type Kind = 'kubernetes' | 'bare-metal'

const KINDS: { value: Kind; label: string }[] = [
  { value: 'kubernetes', label: 'Kubernetes cluster' },
  { value: 'bare-metal', label: 'Bare-metal node' },
]

const KIND_HINT: Record<Kind, string> = {
  kubernetes: 'One agent is installed on the cluster with helm and schedules jobs as pods on its nodes.',
  'bare-metal': 'One agent container runs on the node and starts jobs as containers beside it with docker. Each node is added as a cluster of its own.',
}

const DETACH_NODE = 'docker exec hypothesisloop-agent bare-agent detach && docker wait hypothesisloop-agent && docker rm -f hypothesisloop-agent'

// Both commands read the runtime token with `hl cluster token` when they run, as the command
// `hl cluster add` prints does, so the token is on no page and in no shell history.
function startCommand(kind: Kind, orgID: string, cluster: RegisteredCluster): string {
  const name = shellQuote(cluster.name)
  const token = `RUNTIME_TOKEN="$(hl cluster token ${name} --org ${shellQuote(orgID)})"`
  const api = `API_URL=${shellQuote(cluster.api_url)}`
  if (kind === 'bare-metal') {
    // The same command `hl cluster add --bare-metal` prints (cli/accounts.go bareMetalCommand).
    const image = shellQuote(`${cluster.registry}/hypothesisloop-bare-agent:${cluster.image_tag}`)
    return `${token} docker run -d --name hypothesisloop-agent --restart unless-stopped --hostname "$(hostname)" ` +
      '-v /var/run/docker.sock:/var/run/docker.sock -v /etc/machine-id:/etc/machine-id:ro -v /var/lib/hypothesisloop:/var/lib/hypothesisloop ' +
      `-e CLUSTER_NAME=${name} -e ${api} -e RUNTIME_TOKEN ${image}`
  }
  return `${token} make cluster-agent-up CLUSTER=${name} ${api} METRICS_URL=${shellQuote(cluster.metrics_url)} REGISTRY=${shellQuote(cluster.registry)} TAG=${shellQuote(cluster.image_tag)}`
}

/**
 * The clusters the org's jobs can run on: its own, with the scheduler settings an owner reads and
 * changes, and any the platform provides, each with whether the org uses it for its jobs. With
 * `manage` an owner also turns a cluster off or on for the org's jobs, adds a Kubernetes cluster
 * or a bare-metal node and reads the command that starts its agent.
 */
export function OrgClusters({ manage }: { manage: boolean }) {
  const { org, administers } = useTenant()
  const { data, error, mutate } = useSWR(['org-clusters', org.id], () => fetchOrgClusters(org.id), { refreshInterval: 5_000 })
  const [kind, setKind] = useState<Kind>('kubernetes')
  const [name, setName] = useState('')
  const [busy, setBusy] = useState(false)
  const [actionError, setActionError] = useState('')
  const [registered, setRegistered] = useState<RegisteredCluster | null>(null)
  const [switching, setSwitching] = useState('')
  const manages = manage && administers

  async function setEnabled(cluster: OrgCluster, enabled: boolean) {
    setSwitching(cluster.name)
    setActionError('')
    try {
      await setClusterEnabled(org.id, cluster.name, enabled)
      await mutate()
    } catch (e) {
      setActionError(e instanceof Error ? e.message : String(e))
    } finally {
      setSwitching('')
    }
  }

  // Registering a name the org already owns changes nothing and answers the same addresses, so
  // adding a cluster and reading an existing one's command again are the same call.
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
    <div className="grid gap-3.5">
      {manages && (
        <form className="grid justify-items-start gap-2" onSubmit={add}>
          <Segmented label="Kind of cluster" options={KINDS} value={kind} onChange={setKind} />
          <p className="max-w-[640px] text-xs text-subtle">{KIND_HINT[kind]}</p>
          <div className="flex flex-wrap items-center gap-2">
            <Input
              fieldSize="sm"
              mono
              className="w-60"
              aria-label="Cluster name"
              placeholder={kind === 'kubernetes' ? 'cluster name, e.g. lab-k8s' : 'node name, e.g. gpu-box-1'}
              value={name}
              onChange={e => setName(e.target.value)}
            />
            <Button size="sm" variant="primary" type="submit" loading={busy} disabled={name.trim() === ''}>
              {kind === 'kubernetes' ? 'Add cluster' : 'Add node'}
            </Button>
          </div>
        </form>
      )}
      {actionError && <ErrorMessage>{actionError}</ErrorMessage>}
      {registered && (
        <Secret
          label={`Start the agent of ${registered.name} · ${kind === 'kubernetes' ? 'Kubernetes cluster' : 'bare-metal node'}`}
          value={startCommand(kind, org.id, registered)}
          note={<>
            {kind === 'kubernetes' ? (
              <>
                Run it from a checkout of the repository, with <span className="font-mono">kubectl</span> pointed
                at the cluster and <span className="font-mono">hl</span> signed in as an owner of {org.name}.
              </>
            ) : (
              <>
                Run it on the node. It needs docker there and nothing else, and{' '}
                <span className="font-mono">hl</span> signed in as an owner of {org.name}; without{' '}
                <span className="font-mono">hl</span> on the node, set <span className="font-mono">RUNTIME_TOKEN</span>{' '}
                to what <span className="font-mono">hl cluster token</span> prints. The agent comes back after a
                reboot. To detach the node, run on it:{' '}
                <span className="font-mono">{DETACH_NODE}</span>
              </>
            )}{' '}
            It reads the cluster&apos;s runtime token with <span className="font-mono">hl cluster token</span>{' '}
            when it runs, so the token is not on this page. The kind chosen above selects the command, and{' '}
            <span className="font-mono">Show start command</span> in the cluster&apos;s row brings it back.
          </>}
          dismiss="Hide"
          onDismiss={() => setRegistered(null)}
        />
      )}

      {error && <ErrorMessage>Could not load the clusters of {org.name}: {error.message}</ErrorMessage>}
      {!data && !error && <Loading />}
      {data && data.length === 0 && (
        <EmptyState>
          No cluster is registered for {org.name}, and the platform provides none.
          {manages ? ' Add one above to run jobs on your own hardware.' : ' An owner adds one in Settings.'}
        </EmptyState>
      )}
      {data && data.length > 0 && (
        <Table>
          <thead>
            <tr>
              <th>Cluster</th>
              <th>Status</th>
              <th>Provided by</th>
              <th>Jobs of {org.name}</th>
              <th>Scheduler settings</th>
              {manages && <th />}
            </tr>
          </thead>
          <tbody>
            {data.map(c => (
              <tr key={c.name}>
                <td className="font-mono text-xs font-semibold">
                  {c.name}
                  {c.cluster_id && <div className="text-2xs font-normal text-muted">{c.cluster_id}</div>}
                </td>
                <td><StatusLabel tone={c.connected ? 'success' : 'danger'}>{c.connected ? 'Connected' : 'Disconnected'}</StatusLabel></td>
                <td className="text-xs">
                  {c.owned ? org.name : <Badge tone="accent">Platform</Badge>}
                  {c.shared && <div className="mt-0.5 text-2xs text-muted">shared with every org</div>}
                </td>
                <td>
                  <div className="flex items-center gap-2">
                    {manages && (
                      <Switch
                        label={`Use ${c.name} for the jobs of ${org.name}`}
                        checked={c.enabled}
                        disabled={switching !== ''}
                        onChange={enabled => setEnabled(c, enabled)}
                      />
                    )}
                    <StatusLabel tone={c.enabled ? 'success' : 'neutral'}>{c.enabled ? 'Used' : 'Not used'}</StatusLabel>
                  </div>
                  {!c.enabled && (
                    <div className="mt-0.5 max-w-[220px] text-2xs text-muted">
                      Turned off: no job of {org.name} is placed here. Jobs already running finish.
                    </div>
                  )}
                </td>
                <td>
                  {!c.owned
                    ? <span className="text-xs text-muted">set by the platform</span>
                    : !administers
                      ? <span className="text-xs text-muted">set by an owner of {org.name}</span>
                      : c.cluster_id
                        ? <ClusterSettingsEditor clusterID={c.cluster_id} />
                        : <span className="text-xs text-muted">available once its agent has connected</span>}
                </td>
                {manages && (
                  <td>
                    {c.owned && <Button size="sm" disabled={busy} onClick={() => register(c.name)}>Show start command</Button>}
                  </td>
                )}
              </tr>
            ))}
          </tbody>
        </Table>
      )}
    </div>
  )
}
