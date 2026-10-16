'use client'

import { Fragment, useState, type ReactNode } from 'react'
import useSWR from 'swr'
import { ChevronDown, ChevronUp, GripVertical } from 'lucide-react'
import { fetchOrgClusters, registerCluster, removeCluster, setClusterEnabled, setClusterOrder, updateCluster } from '@/lib/api/clusters'
import type { ClusterKind as Kind, OrgCluster, RegisteredCluster } from '@/types/clusters'
import { useTenant } from '@/lib/tenant'
import { formatDate, relTime } from '@/lib/format'
import { ClusterSettingsEditor } from '@/components/cluster-settings-editor'
import { Badge, StatusLabel } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { CopyButton } from '@/components/ui/copy-button'
import { ConfirmDialog } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Table } from '@/components/ui/table'
import { Secret } from '@/components/ui/secret'
import { Switch } from '@/components/ui/switch'
import { EmptyState } from '@/components/ui/empty-state'
import { Segmented } from '@/components/ui/segmented'
import { Inline, Stack } from '@/components/ui/stack'
import { Loading, ErrorMessage } from '@/components/ui/status-message'
import { Code, Heading, Text } from '@/components/ui/text'
import { keys } from '@/lib/keys'
import { POLL } from '@/lib/poll'

// What runs the jobs. The API records no kind: a cluster is a name, and its kind is whichever agent
// connects under it. Registering answers the command for each, and the choice here shows one.
const KINDS: { value: Kind; label: string }[] = [
  { value: 'kubernetes', label: 'Kubernetes cluster' },
  { value: 'bare_metal', label: 'Bare-metal node' },
]

function GuidePart({ title, children }: { title: string; children: ReactNode }) {
  return (
    <Stack gap="xs" as="section" aria-label={title}>
      <Heading level={3}>{title}</Heading>
      {children}
    </Stack>
  )
}

const guideList = 'list-disc space-y-1 pl-5 text-xs text-muted'

/** What running the command needs, what it puts where, and how to check, upgrade and remove it. */
function InstallGuide({ kind, registered }: { kind: Kind; registered: RegisteredCluster }) {
  if (kind === 'kubernetes') {
    return (
      <div className="grid w-full gap-4 md:grid-cols-2">
        <GuidePart title="Before you run it">
          <ul className={guideList}>
            <li>Kubernetes 1.34 or newer, <Code>helm</Code> 3.17 or newer, and <Code>kubectl</Code> access that may create namespaces, PriorityClasses and cluster roles.</li>
            <li>Every node reaches <Code>{registered.api_url}</Code> and <Code>{registered.metrics_url}</Code> and pulls images from <Code>{registered.registry}</Code>. Nothing connects into the cluster. Pods resolve these names with the cluster&apos;s DNS, which does not know tailnet names: a platform on a tailnet is named by its IP.</li>
            <li>Accelerators need their device plugin or DRA driver: the NVIDIA GPU Operator, the AMD GPU device plugin with its node labeller, or the Tenstorrent DRA driver. Every cluster also offers its CPU cores.</li>
          </ul>
        </GuidePart>
        <GuidePart title="What it installs">
          <ul className={guideList}>
            <li>In the namespace <Code>hypothesisloop</Code>: the cluster-agent Deployment, the node-agent DaemonSet on every node, and the Secret <Code>hypothesisloop-runtime-token</Code>.</li>
            <li>The namespace <Code>hypothesisloop-jobs</Code>, where each job runs as a Kubernetes Job, and the PriorityClasses <Code>hypothesisloop-jobs-guaranteed</Code> and <Code>hypothesisloop-jobs-burst</Code>.</li>
            <li>Permissions to create and delete Jobs, pods, Services, Secrets and ResourceClaimTemplates in <Code>hypothesisloop-jobs</Code> only, and to read nodes, pods and DRA devices.</li>
          </ul>
        </GuidePart>
        <GuidePart title="Steps">
          <ol className="list-decimal space-y-1 pl-5 text-xs text-muted">
            <li>To preview every object first, add helm&apos;s own flag <Code>--dry-run=server</Code> at the end of the command. It does not check that the cluster reaches the platform or pulls the images: <Code>install.sh --dry-run</Code> from a checkout does.</li>
            <li>Run the command and paste the runtime token when it asks.</li>
            <li>Check <Code>kubectl -n hypothesisloop get pods</Code>. The cluster shows Connected in the list below within a minute.</li>
          </ol>
        </GuidePart>
        <GuidePart title="Settings, upgrade and removal">
          <ul className={guideList}>
            <li>Its settings: <Code>helm get values hypothesisloop-cluster-agent -n hypothesisloop</Code></li>
            <li>To upgrade, run the command again.</li>
            <li>To remove it: <Code>helm uninstall hypothesisloop-cluster-agent -n hypothesisloop</Code>, which first sends the cluster&apos;s jobs back to the queue, then <Code>kubectl delete namespace hypothesisloop</Code>.</li>
          </ul>
        </GuidePart>
      </div>
    )
  }
  return (
    <div className="grid w-full gap-4 md:grid-cols-2">
      <GuidePart title="Before you run it">
        <ul className={guideList}>
          <li>Linux with systemd, root, and docker or podman. Each node is a cluster of its own.</li>
          <li>NVIDIA GPUs need the driver and the NVIDIA Container Toolkit. Tenstorrent cards need the tt-kmd driver and 1 GiB hugepages. Every node also offers its CPU cores.</li>
          <li>The node reaches <Code>{registered.api_url}</Code> and pulls images from <Code>{registered.registry}</Code>. Nothing connects into the node.</li>
        </ul>
      </GuidePart>
      <GuidePart title="What it installs">
        <ul className={guideList}>
          <li><Code>/usr/local/bin/bare-agent</Code>, the agent.</li>
          <li><Code>/etc/hypothesisloop/bare-agent.env</Code>, the node&apos;s one config file: cluster name, API address and token, readable by root only.</li>
          <li>The systemd unit <Code>hypothesisloop-agent.service</Code>, started at boot. Logs: <Code>journalctl -u hypothesisloop-agent -f</Code></li>
        </ul>
      </GuidePart>
      <GuidePart title="Steps">
        <ol className="list-decimal space-y-1 pl-5 text-xs text-muted">
          <li>To check the node and see what it would write, add <Code>--dry-run</Code> at the end of the command.</li>
          <li>Run the command on the node and paste the runtime token when it asks.</li>
          <li>The node shows Connected in the list below within a minute.</li>
        </ol>
      </GuidePart>
      <GuidePart title="Upgrade and removal">
        <ul className={guideList}>
          <li>To upgrade, run the command of a newer version.</li>
          <li>To remove it, on the node: <Code>sudo bare-agent detach</Code>, which stops its jobs and sends them back to the queue, then <Code>sudo bare-agent uninstall</Code>.</li>
        </ul>
      </GuidePart>
    </div>
  )
}

/**
 * Whether the cluster's agent is connected, when it was last heard from, the version it runs and
 * whether that is the platform's own, and what the platform last refused it with.
 */
function ClusterStatus({ cluster: c }: { cluster: OrgCluster }) {
  return (
    <Stack gap="xs" start>
      <StatusLabel tone={c.connected ? 'success' : 'danger'}>{c.connected ? 'Connected' : 'Disconnected'}</StatusLabel>
      {!c.connected && (
        <Text as="div" variant="meta">
          {c.last_contact_at ? <>Last contact <span title={formatDate(c.last_contact_at)}>{relTime(c.last_contact_at)}</span></> : 'Not heard from in 30 days'}
          {c.enabled && ' · skipped until it reconnects'}
        </Text>
      )}
      {c.agent_version && (
        <Text as="div" variant="meta">Agent <Code>{c.agent_version}</Code></Text>
      )}
      {c.version_skew && (
        <Badge tone="warning" title="The agent runs another version than the platform. Run the start command again to upgrade it.">Not the platform&apos;s version</Badge>
      )}
      {c.last_error && (
        <Text as="div" variant="meta" className="max-w-72 text-danger" title={formatDate(c.last_error_at)}>
          Last refused{c.last_error_at ? ` ${relTime(c.last_error_at)}` : ''}: {c.last_error}
        </Text>
      )}
    </Stack>
  )
}

/** The command that attaches a registered cluster, for the kind chosen in it, and the token it asks for. */
function StartCommand({ registered, onHide }: { registered: RegisteredCluster; onHide: () => void }) {
  const [kind, setKind] = useState<Kind>('kubernetes')
  return (
    <Stack gap="sm" start>
      <Segmented label="Kind of cluster" options={KINDS} value={kind} onChange={setKind} />
      <div className="w-full">
        <Secret
          label={`Start the agent of ${registered.name} · ${kind === 'kubernetes' ? 'Kubernetes cluster' : 'bare-metal node'}`}
          value={registered.attach_commands[kind]}
          note={<>
            {kind === 'kubernetes'
              ? 'Run it where kubectl reaches the cluster.'
              : 'Run it on the node.'}{' '}
            It asks for the runtime token below, so the machine needs neither <Code>hl</Code> nor
            an API key. The command holds no token, and <Code>Show start command</Code> in
            the cluster&apos;s row brings it back.
            <span className="mt-2 grid gap-1">
              <span>Runtime token of {registered.name}, which the command asks for:</span>
              <span className="flex flex-wrap items-center gap-2">
                <Code className="select-all break-all text-foreground" data-testid="runtime-token">{registered.runtime_token}</Code>
                <CopyButton value={registered.runtime_token} />
              </span>
            </span>
          </>}
          dismiss="Hide"
          onDismiss={onHide}
        />
      </div>
      <InstallGuide kind={kind} registered={registered} />
    </Stack>
  )
}

/** The buttons that move one of the org's own clusters up or down its order, beside the grip a row is dragged by. */
function MoveButtons({ cluster, index, of, onMove, disabled }: { cluster: OrgCluster; index: number; of: number; onMove: (to: number) => void; disabled: boolean }) {
  return (
    <span className="flex items-center gap-1">
      <GripVertical aria-hidden="true" size={14} className="cursor-grab text-subtle" />
      <Button size="icon" variant="ghost" aria-label={`Move ${cluster.name} up`} disabled={disabled || index === 0} onClick={() => onMove(index - 1)}>
        <ChevronUp aria-hidden="true" size={14} />
      </Button>
      <Button size="icon" variant="ghost" aria-label={`Move ${cluster.name} down`} disabled={disabled || index === of - 1} onClick={() => onMove(index + 1)}>
        <ChevronDown aria-hidden="true" size={14} />
      </Button>
    </span>
  )
}

/**
 * Where a job of the org tries the cluster: its place, which it may share with others the job
 * tries together, or that it is skipped while turned off.
 */
function Place({ place, shared }: { place?: number; shared: boolean }) {
  if (!place) return <Text as="span" variant="note">Off</Text>
  return (
    <Stack gap="2xs" start>
      <Code>{place}</Code>
      {shared && <Text as="div" variant="meta" className="whitespace-nowrap">tied, most room first</Text>}
    </Stack>
  )
}

/**
 * Where a job of the org goes right now: the first place with a connected cluster. A job that does
 * not fit there moves down the order.
 */
function NextPlacement({ clusters }: { clusters: OrgCluster[] }) {
  const placed = clusters.filter(c => c.place)
  if (placed.length === 0) return <Text variant="note">Every cluster is turned off, so jobs wait in the queue.</Text>
  const first = Math.min(...placed.filter(c => c.connected).map(c => c.place!))
  if (!Number.isFinite(first)) return <Text variant="note">No cluster in use is connected, so jobs wait in the queue until one is.</Text>
  const names = placed.filter(c => c.place === first && c.connected).map(c => c.name)
  return (
    <Text variant="note">
      Jobs go to the first connected cluster below with room: right now{' '}
      {names.map((n, i) => <Fragment key={n}>{i > 0 && ' or '}<Code>{n}</Code></Fragment>)}.
      {' '}A job that does not fit there moves down the order.
    </Text>
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
