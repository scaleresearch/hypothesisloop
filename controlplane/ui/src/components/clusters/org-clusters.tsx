'use client'

import { Fragment, useState } from 'react'
import useSWR from 'swr'
import { fetchOrgClusters, registerCluster, removeCluster, setClusterEnabled, setClusterOrder, updateCluster } from '@/lib/api/clusters'
import type { OrgCluster, RegisteredCluster } from '@/types/clusters'
import { useTenant } from '@/lib/tenant'
import { ClusterSettingsEditor } from '@/components/clusters/settings-editor'
import { ClusterStatus } from '@/components/clusters/cluster-status'
import { MoveButtons, NextPlacement, Place } from '@/components/clusters/placement'
import { StartCommand } from '@/components/clusters/start-command'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { ConfirmDialog } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Table } from '@/components/ui/table'
import { Switch } from '@/components/ui/switch'
import { EmptyState } from '@/components/ui/empty-state'
import { Inline, Stack } from '@/components/ui/stack'
import { Loading, ErrorMessage } from '@/components/ui/status-message'
import { Text } from '@/components/ui/text'
import { keys } from '@/lib/keys'
import { POLL } from '@/lib/poll'

/**
 * The clusters the org's jobs can run on: its own, in the order a job is placed on them, then any
 * the platform provides, which are used last. An owner adds a cluster and reads the command that
 * starts its agent, reorders the org's own by dragging a row or with its up and down buttons,
 * which the API writes as one order, turns a cluster off or on for the org's jobs, and under
 * Advanced changes a cluster's scheduler settings or removes it. Staff share one there too.
 */
export function OrgClusters() {
  const { org, administers, staff } = useTenant()
  const { data, error, mutate } = useSWR(keys.orgClusters(org.id), () => fetchOrgClusters(org.id), { refreshInterval: POLL.live })
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
  // While a move is written the list shows the order asked for, each in a place of its own as the
  // write gives it; a refusal puts it back.
  const shownOwn = moving
    ? moving.map(n => own.find(c => c.name === n)).filter((c): c is OrgCluster => !!c).map((c, i) => ({ ...c, place: c.enabled ? i + 1 : undefined }))
    : own
  const shown = [...shownOwn, ...others]
  const tied = (c: OrgCluster) => !!c.place && shown.filter(o => o.place === c.place).length > 1
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

  function row(c: OrgCluster, index?: number) {
    const orders = administers && index !== undefined
    return (
      <Fragment key={c.name}>
        <tr
          data-cluster={c.name}
          draggable={orders && switching === ''}
          onDragStart={orders ? () => setDragged(c.name) : undefined}
          onDragOver={orders && dragged ? e => e.preventDefault() : undefined}
          onDrop={orders && dragged ? () => { void move(dragged, index); setDragged('') } : undefined}
          onDragEnd={() => setDragged('')}
        >
          <td className="text-xs">
            <span className="flex items-center gap-2">
              <Place place={c.place} shared={tied(c)} />
              {orders && <MoveButtons cluster={c} index={index} of={shownOwn.length} onMove={to => move(c.name, to)} disabled={switching !== ''} />}
            </span>
          </td>
          <td className="font-mono text-xs font-medium">{c.name}</td>
          <td><ClusterStatus cluster={c} /></td>
          <td className="text-xs">
            {c.owned ? org.name : <Badge tone="neutral">Platform</Badge>}
            {c.shared && <Text as="div" variant="meta">shared with every org</Text>}
          </td>
          <td>
            <Switch
              label={`Use ${c.name} for the jobs of ${org.name}`}
              checked={c.enabled}
              disabled={!administers || switching !== ''}
              onChange={enabled => change(c.name, () => setClusterEnabled(org.id, c.name, enabled))}
            />
            {!c.enabled && <Text as="div" variant="meta" className="whitespace-nowrap">Off: no new jobs go here.</Text>}
          </td>
          {administers && (
            <td>
              {c.owned && (
                <Inline>
                  <Button size="sm" disabled={busy} onClick={() => register(c.name)}>Show start command</Button>
                  <Button size="sm" variant="ghost" aria-expanded={advanced === c.name} onClick={() => setAdvanced(advanced === c.name ? '' : c.name)}>Advanced</Button>
                </Inline>
              )}
            </td>
          )}
        </tr>
        {administers && c.owned && advanced === c.name && (
          <tr>
            <td colSpan={columns} aria-label={`Advanced settings of ${c.name}`}>
              <Stack start className="py-1">
                <ClusterSettingsEditor key={c.name} orgID={org.id} cluster={c} onSaved={mutate} />
                <Inline gap="md">
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
                </Inline>
              </Stack>
            </td>
          </tr>
        )}
      </Fragment>
    )
  }

  return (
    <Stack>
      {administers && (
        <Inline as="form" onSubmit={add}>
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
          <Text as="span" variant="hint">A Kubernetes cluster, or one bare-metal node.</Text>
        </Inline>
      )}
      {actionError && <ErrorMessage>{actionError}</ErrorMessage>}
      {registered && <StartCommand key={registered.name} registered={registered} onHide={() => setRegistered(null)} />}

      {error && <ErrorMessage>Could not load the clusters of {org.name}: {error.message}</ErrorMessage>}
      {!data && !error && <Loading />}
      {data && data.length === 0 && (
        <EmptyState>
          No cluster is registered for {org.name}, and the platform provides none.
          {!administers && ' An owner adds one here.'}
        </EmptyState>
      )}
      {data && data.length > 0 && (
        <Stack gap="sm">
          <NextPlacement clusters={shown} />
          <Table>
            <thead>
              <tr>
                <th>Order</th>
                <th>Cluster</th>
                <th>Status</th>
                <th>Provided by</th>
                <th title={`Whether jobs of ${org.name} are placed on the cluster`}>Use for jobs</th>
                {administers && <th />}
              </tr>
            </thead>
            <tbody>
              <tr>
                <th colSpan={columns} scope="colgroup" className="bg-surface-inset py-2 text-foreground">Clusters of {org.name}, tried first, in the order {administers ? 'you set' : 'an owner sets'}</th>
              </tr>
              {shownOwn.length === 0 && (
                <tr>
                  <td colSpan={columns}>
                    <Text as="span" variant="note">
                      None yet.
                    </Text>
                  </td>
                </tr>
              )}
              {shownOwn.map((c, index) => row(c, index))}
              {others.length > 0 && (
                <tr>
                  <th colSpan={columns} scope="colgroup" className="bg-surface-inset py-2 text-foreground">Platform compute, shared with every org, tried after {org.name}&apos;s own</th>
                </tr>
              )}
              {others.map(c => row(c))}
            </tbody>
          </Table>
        </Stack>
      )}
      <ConfirmDialog
        open={removing !== ''}
        title={`Remove ${removing}?`}
        confirmLabel="Remove cluster"
        onClose={() => setRemoving('')}
        onConfirm={async () => {
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
    </Stack>
  )
}
