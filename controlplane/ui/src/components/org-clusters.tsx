'use client'

import { Fragment, useState } from 'react'
import useSWR from 'swr'
import { ChevronDown, ChevronUp, GripVertical } from 'lucide-react'
import { fetchOrgClusters, registerCluster, removeCluster, setClusterEnabled, setClusterOrder, updateCluster } from '@/lib/api'
import type { ClusterKind as Kind, OrgCluster, RegisteredCluster } from '@/types'
import { useTenant } from '@/lib/tenant'
import { ClusterSettingsEditor } from '@/components/cluster-settings-editor'
import { Badge, StatusLabel } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { CopyButton } from '@/components/ui/copy-button'
import { ConfirmDialog } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Table } from '@/components/ui/table'
import { Secret } from '@/components/ui/secret'
import { Switch } from '@/components/ui/switch'
import { Segmented } from '@/components/ui/tabs'
import { Loading, ErrorMessage, EmptyState } from '@/components/ui/status-message'

// What runs the jobs. The API records no kind: a cluster is a name, and its kind is whichever agent
// connects under it. Registering answers the command for each, and the choice here shows one.
const KINDS: { value: Kind; label: string }[] = [
  { value: 'kubernetes', label: 'Kubernetes cluster' },
  { value: 'bare_metal', label: 'Bare-metal node' },
]

/** The command that attaches a registered cluster, for the kind chosen in it, and the token it asks for. */
function StartCommand({ registered, onHide }: { registered: RegisteredCluster; onHide: () => void }) {
  const [kind, setKind] = useState<Kind>('kubernetes')
  return (
    <div className="grid justify-items-start gap-2">
      <Segmented label="Kind of cluster" options={KINDS} value={kind} onChange={setKind} />
      <div className="w-full">
        <Secret
          label={`Start the agent of ${registered.name} · ${kind === 'kubernetes' ? 'Kubernetes cluster' : 'bare-metal node'}`}
          value={registered.attach_commands[kind]}
          note={<>
            {kind === 'kubernetes' ? (
              <>
                Run it with <span className="font-mono">helm</span> 3.17 or newer and{' '}
                <span className="font-mono">kubectl</span> pointed at the cluster. It installs one agent,
                which schedules jobs as pods on the cluster&apos;s nodes, or upgrades it, and asks for the
                runtime token below; the machine needs neither <span className="font-mono">hl</span> nor
                an API key. To detach the
                cluster: <span className="font-mono">helm uninstall hypothesisloop-cluster-agent -n hypothesisloop</span>
              </>
            ) : (
              <>
                Run it on the node, which needs docker or podman, systemd and root. It installs the agent
                as a unit that starts at boot and starts jobs as containers, and asks for the runtime token
                below; the node needs neither <span className="font-mono">hl</span> nor an API key. Each
                node is added as a cluster of its own. The command of a newer image tag upgrades the
                agent. To detach the node, run on it <span className="font-mono">sudo bare-agent detach</span>,
                then <span className="font-mono">sudo bare-agent uninstall</span> to remove the agent.
              </>
            )}{' '}
            The command holds no token, and <span className="font-mono">Show start command</span> in
            the cluster&apos;s row brings it back.
            <span className="mt-2 grid gap-1">
              <span>Runtime token of {registered.name}, which the command asks for:</span>
              <span className="flex flex-wrap items-center gap-2">
                <span className="select-all break-all font-mono text-foreground" data-testid="runtime-token">{registered.runtime_token}</span>
                <CopyButton value={registered.runtime_token} />
              </span>
            </span>
          </>}
          dismiss="Hide"
          onDismiss={onHide}
        />
      </div>
    </div>
  )
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
 * The clusters the org's jobs can run on: its own, in the order a job is placed on them, then any
 * the platform provides, which are used last. An owner adds a cluster and reads the command that
 * starts its agent, reorders the org's own by dragging a row or with its up and down buttons,
 * which the API writes as one order, turns a cluster off or on for the org's jobs, and under
 * Advanced changes a cluster's scheduler settings or removes it. Staff share one there too.
 */
export function OrgClusters() {
  const { org, administers, staff } = useTenant()
  const { data, error, mutate } = useSWR(['org-clusters', org.id], () => fetchOrgClusters(org.id), { refreshInterval: 5_000 })
  const [name, setName] = useState('')
  const [busy, setBusy] = useState(false)
  const [actionError, setActionError] = useState('')
  const [registered, setRegistered] = useState<RegisteredCluster | null>(null)
  const [switching, setSwitching] = useState('')
  const [moving, setMoving] = useState<string[] | null>(null)
  const [dragged, setDragged] = useState('')
  const [advanced, setAdvanced] = useState('')
  const [removing, setRemoving] = useState('')

  const own = (data ?? []).filter(c => c.owned && !c.shared)
  const others = (data ?? []).filter(c => !c.owned || c.shared)
  // While a move is written the list shows the order asked for; a refusal puts it back.
  const shownOwn = moving ? moving.map(n => own.find(c => c.name === n)).filter((c): c is OrgCluster => !!c) : own
  const columns = administers ? 6 : 5

  async function change(cluster: string, act: () => Promise<unknown>) {
    setSwitching(cluster)
    setActionError('')
    try {
      await act()
    } catch (e) {
      setActionError(e instanceof Error ? e.message : String(e))
    }
    await mutate()
    setSwitching('')
  }

  async function move(name: string, to: number) {
    const from = own.findIndex(c => c.name === name)
    if (from < 0 || to < 0 || to >= own.length || from === to) return
    const order = own.map(c => c.name)
    order.splice(to, 0, ...order.splice(from, 1))
    setMoving(order)
    await change(name, () => setClusterOrder(org.id, order))
    setMoving(null)
  }

  // Registering a name the org already owns changes nothing and answers the same commands, so
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
      {administers && (
        <form className="flex flex-wrap items-center gap-2" onSubmit={add}>
          <Input
            fieldSize="sm"
            mono
            className="w-60"
            aria-label="Cluster name"
            placeholder="name, e.g. lab-k8s or gpu-box-1"
            value={name}
            onChange={e => setName(e.target.value)}
          />
          <Button size="sm" variant="primary" type="submit" loading={busy} disabled={name.trim() === ''}>Add cluster</Button>
          <span className="text-xs text-subtle">A Kubernetes cluster, or one bare-metal node.</span>
        </form>
      )}
      {actionError && <ErrorMessage>{actionError}</ErrorMessage>}
      {registered && <StartCommand key={registered.name} registered={registered} onHide={() => setRegistered(null)} />}

      {error && <ErrorMessage>Could not load the clusters of {org.name}: {error.message}</ErrorMessage>}
      {!data && !error && <Loading />}
      {data && data.length === 0 && (
        <EmptyState>
          No cluster is registered for {org.name}, and the platform provides none.
          {administers ? ' Add one above to run jobs on your own hardware.' : ' An owner adds one here.'}
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
              {administers && <th />}
            </tr>
          </thead>
          <tbody>
            {[...shownOwn, ...others].map((c, place) => {
              const orders = administers && place < shownOwn.length
              return (
              <Fragment key={c.name}>
              <tr
                draggable={orders && switching === ''}
                onDragStart={orders ? () => setDragged(c.name) : undefined}
                onDragOver={orders && dragged ? e => e.preventDefault() : undefined}
                onDrop={orders && dragged ? () => { void move(dragged, place); setDragged('') } : undefined}
                onDragEnd={() => setDragged('')}
              >
                <td className="font-mono text-xs font-semibold">{c.name}</td>
                <td><StatusLabel tone={c.connected ? 'success' : 'danger'}>{c.connected ? 'Connected' : 'Disconnected'}</StatusLabel></td>
                <td className="text-xs">
                  {c.owned ? org.name : <Badge tone="neutral">Platform</Badge>}
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
                  <Switch
                    label={`Use ${c.name} for the jobs of ${org.name}`}
                    checked={c.enabled}
                    disabled={!administers || switching !== ''}
                    onChange={enabled => change(c.name, () => setClusterEnabled(org.id, c.name, enabled))}
                  />
                  {!c.enabled && (
                    <div className="mt-0.5 max-w-[220px] text-2xs text-muted">
                      Turned off: no job of {org.name} is placed here. Jobs already running finish.
                    </div>
                  )}
                </td>
                {administers && (
                  <td>
                    {c.owned && (
                      <div className="flex flex-wrap items-center gap-2">
                        <Button size="sm" disabled={busy} onClick={() => register(c.name)}>Show start command</Button>
                        <Button size="sm" variant="ghost" aria-expanded={advanced === c.name} onClick={() => setAdvanced(advanced === c.name ? '' : c.name)}>Advanced</Button>
                      </div>
                    )}
                  </td>
                )}
              </tr>
              {administers && c.owned && advanced === c.name && (
                <tr>
                  <td colSpan={columns} aria-label={`Advanced settings of ${c.name}`}>
                    <div className="grid justify-items-start gap-3 py-1">
                      <ClusterSettingsEditor key={c.name} orgID={org.id} cluster={c} onSaved={mutate} />
                      <div className="flex flex-wrap items-center gap-3">
                        {staff && (
                          <label className="flex items-center gap-2 text-xs">
                            <Switch
                              label={`Share ${c.name} with every org`}
                              checked={c.shared}
                              disabled={switching !== ''}
                              onChange={shared => change(c.name, () => updateCluster(org.id, c.name, { shared }))}
                            />
                            Shared with every org · staff only
                          </label>
                        )}
                        <Button size="sm" variant="danger" disabled={switching !== ''} onClick={() => setRemoving(c.name)}>Remove cluster</Button>
                      </div>
                    </div>
                  </td>
                </tr>
              )}
              </Fragment>
              )
            })}
          </tbody>
        </Table>
      )}
      <ConfirmDialog
        open={removing !== ''}
        title={`Remove ${removing}?`}
        confirmLabel="Remove cluster"
        onCancel={() => setRemoving('')}
        onConfirm={() => {
          const cluster = removing
          setRemoving('')
          setAdvanced('')
          void change(cluster, () => removeCluster(org.id, cluster))
        }}
      >
        Every job placed on it goes back to the queue and is placed on another cluster, keeping its
        retries. Its runtime token is refused from then on, so stop its agent. The name is free to
        register again.
      </ConfirmDialog>
    </div>
  )
}
