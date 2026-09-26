'use client'

import { useState } from 'react'
import useSWR from 'swr'
import { ChevronDown, ChevronUp, GripVertical } from 'lucide-react'
import { fetchOrgClusters, registerCluster, setClusterEnabled, setClusterPriority } from '@/lib/api'
import { moved, priorityWrites } from '@/lib/clusters'
import type { OrgCluster, RegisteredCluster } from '@/types'
import { type ClusterKind as Kind, startCommand } from '@/lib/attach-command'
import { useTenant } from '@/lib/tenant'
import { ClusterSettingsEditor } from '@/components/cluster-settings-editor'
import { Badge, StatusLabel } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { CopyButton } from '@/components/ui/copy-button'
import { Input } from '@/components/ui/input'
import { Table } from '@/components/ui/table'
import { Secret } from '@/components/ui/secret'
import { Switch } from '@/components/ui/switch'
import { Segmented } from '@/components/ui/tabs'
import { Loading, ErrorMessage, EmptyState } from '@/components/ui/status-message'

// What runs the jobs. The API records no kind: a cluster is a name, and its kind is whichever agent
// connects under it. So the choice here only selects the command that starts that agent.

const KINDS: { value: Kind; label: string }[] = [
  { value: 'kubernetes', label: 'Kubernetes cluster' },
  { value: 'bare-metal', label: 'Bare-metal node' },
]

const KIND_HINT: Record<Kind, string> = {
  kubernetes: 'One agent is installed on the cluster with helm and schedules jobs as pods on its nodes.',
  'bare-metal': 'One agent runs on the node as a systemd unit and starts jobs as containers with docker or podman. Each node is added as a cluster of its own.',
}

/**
 * Where a cluster the org registered stands among its own, and the two ways to move it: the
 * buttons here, and dragging its row.
 */
function ClusterOrder({ cluster, place, of, onMove, disabled }: { cluster: OrgCluster; place: number; of: number; onMove: (to: number) => void; disabled: boolean }) {
  return (
    <span className="flex items-center gap-1">
      <GripVertical aria-hidden="true" size={14} className="cursor-grab text-subtle" />
      <span className="w-4 font-mono">{place + 1}</span>
      <Button size="icon" variant="ghost" aria-label={`Move ${cluster.name} up`} disabled={disabled || place === 0} onClick={() => onMove(place - 1)}>
        <ChevronUp aria-hidden="true" size={14} />
      </Button>
      <Button size="icon" variant="ghost" aria-label={`Move ${cluster.name} down`} disabled={disabled || place === of - 1} onClick={() => onMove(place + 1)}>
        <ChevronDown aria-hidden="true" size={14} />
      </Button>
    </span>
  )
}

/**
 * The clusters the org's jobs can run on: its own, with the scheduler settings an owner reads and
 * changes, in the order a job is placed on them, then any the platform provides, which are used
 * last, each with whether the org uses it for its jobs. An owner reorders the org's own by
 * dragging a row or with its up and down buttons, which writes priorities (priorityWrites). With
 * `manage` an owner also turns a cluster off or on for the org's jobs, adds a Kubernetes cluster
 * or a bare-metal node and reads the command that starts its agent.
 */
export function OrgClusters({ manage }: { manage: boolean }) {
  const { org, administers } = useTenant()
  const { data, error, mutate } = useSWR(['org-clusters', org.id], () => fetchOrgClusters(org.id), { refreshInterval: 5_000 })
  const [kind, setKind] = useState<Kind>('kubernetes')
  const [name, setName] = useState('')
  const [cpuClass, setCPUClass] = useState('')
  const [busy, setBusy] = useState(false)
  const [actionError, setActionError] = useState('')
  const [registered, setRegistered] = useState<RegisteredCluster | null>(null)
  const [switching, setSwitching] = useState('')
  const [moving, setMoving] = useState<string[] | null>(null)
  const [dragged, setDragged] = useState('')
  const manages = manage && administers

  // The org's own clusters as the API orders them, highest priority first, then the rest.
  const own = (data ?? []).filter(c => c.owned && !c.shared)
  const others = (data ?? []).filter(c => !c.owned || c.shared)
  // While a move is written the list shows the order asked for; a refusal puts it back.
  const shownOwn = moving ? moving.map(n => own.find(c => c.name === n)).filter((c): c is OrgCluster => !!c) : own

  async function move(name: string, to: number) {
    const from = own.findIndex(c => c.name === name)
    if (from < 0 || to < 0 || to >= own.length || from === to) return
    const order = moved(own, from, to)
    setMoving(order.map(c => c.name))
    setSwitching(name)
    setActionError('')
    try {
      for (const write of priorityWrites(order, name)) await setClusterPriority(org.id, write.name, write.priority)
    } catch (e) {
      setActionError(e instanceof Error ? e.message : String(e))
    }
    // Reread either way: a refusal part way through a respacing leaves what was written.
    await mutate()
    setMoving(null)
    setSwitching('')
  }

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
            <Input
              fieldSize="sm"
              mono
              className="w-48"
              aria-label="CPU class"
              title="Optional. The cluster then offers its cores to jobs that need no accelerator, as the type cpu.hypothesisloop.dev/class=<name>, which the platform's catalog must price."
              placeholder="CPU class, optional"
              value={cpuClass}
              onChange={e => setCPUClass(e.target.value)}
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
          value={startCommand(kind, registered, cpuClass.trim())}
          note={<>
            {kind === 'kubernetes' ? (
              <>
                Run it with <span className="font-mono">helm</span> 3.17 or newer and{' '}
                <span className="font-mono">kubectl</span> pointed at the cluster. It installs the runtime&apos;s
                chart, or upgrades it, and asks for the runtime token below; the machine needs neither{' '}
                <span className="font-mono">hl</span> nor an API key. To detach the
                cluster: <span className="font-mono">helm uninstall hypothesisloop-cluster-agent -n hypothesisloop</span>
              </>
            ) : (
              <>
                Run it on the node, which needs docker or podman, systemd and root. It installs the agent
                as a unit that starts at boot and asks for the runtime token below; the node needs neither{' '}
                <span className="font-mono">hl</span> nor an API key. The command of a newer image tag upgrades
                the agent. To detach the node, run on it <span className="font-mono">sudo bare-agent detach</span>,
                then <span className="font-mono">sudo bare-agent uninstall</span> to remove the agent.
              </>
            )}{' '}
            The command holds no token. The kind chosen above selects the command, and{' '}
            <span className="font-mono">Show start command</span> in the cluster&apos;s row brings it back.
            <span className="mt-2 grid gap-1">
              <span>Runtime token of {registered.name}, which the command asks for:</span>
              <span className="flex flex-wrap items-center gap-2">
                <span className="select-all break-all font-mono text-foreground" data-testid="runtime-token">{registered.runtime_token}</span>
                <CopyButton value={registered.runtime_token} />
              </span>
            </span>
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
              <th>Order</th>
              <th>Jobs of {org.name}</th>
              <th>Scheduler settings</th>
              {manages && <th />}
            </tr>
          </thead>
          <tbody>
            {[...shownOwn, ...others].map((c, place) => {
              const orders = manages && place < shownOwn.length
              return (
              <tr
                key={c.name}
                draggable={orders && switching === ''}
                onDragStart={orders ? () => setDragged(c.name) : undefined}
                onDragOver={orders && dragged ? e => e.preventDefault() : undefined}
                onDrop={orders && dragged ? () => { void move(dragged, place); setDragged('') } : undefined}
                onDragEnd={() => setDragged('')}
              >
                <td className="font-mono text-xs font-semibold">
                  {c.name}
                  {c.cluster_id && <div className="text-2xs font-normal text-muted">{c.cluster_id}</div>}
                </td>
                <td><StatusLabel tone={c.connected ? 'success' : 'danger'}>{c.connected ? 'Connected' : 'Disconnected'}</StatusLabel></td>
                <td className="text-xs">
                  {c.owned ? org.name : <Badge tone="accent">Platform</Badge>}
                  {c.shared && <div className="mt-0.5 text-2xs text-muted">shared with every org</div>}
                </td>
                <td className="text-xs">
                  {place >= shownOwn.length
                    ? <span className="text-muted">used last, after {org.name}&apos;s own</span>
                    : orders
                      ? <ClusterOrder cluster={c} place={place} of={shownOwn.length} onMove={to => move(c.name, to)} disabled={switching !== ''} />
                      : <span className="font-mono">{place + 1}</span>}
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
              )
            })}
          </tbody>
        </Table>
      )}
    </div>
  )
}
