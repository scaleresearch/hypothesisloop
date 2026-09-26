'use client'

import { useState } from 'react'
import useSWR from 'swr'
import { fetchOrgClusters, registerCluster, setClusterEnabled, setClusterPriority } from '@/lib/api'
import type { OrgCluster, RegisteredCluster } from '@/types'
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
  'bare-metal': 'One agent runs on the node as a systemd unit and starts jobs as containers with docker or podman. Each node is added as a cluster of its own.',
}

// What `hl cluster add --bare-metal` prints (cli/accounts.go bareMetalScript). It runs on the node
// as root: the node's engine copies the agent out of the image that carries it, and the agent
// installs itself as a systemd unit and asks for the runtime token. $1 is the cluster name, $2 the
// API address, $3 the image that carries the agent, $4 the job image and $5, when given, the CPU
// class the node offers its cores as (hl cluster add --cpu-class).
const BARE_METAL_SCRIPT =
  'engine="$(command -v docker || command -v podman)" || { echo "docker or podman is needed on this node" >&2; exit 1; }; ' +
  'staged="$("$engine" create "$3")" && "$engine" cp "$staged:/usr/local/bin/bare-agent" /usr/local/bin/bare-agent.new && "$engine" rm "$staged" >/dev/null && ' +
  'exec /usr/local/bin/bare-agent.new install --cluster "$1" --api-url "$2" --job-image "$4" ${5:+--cpu-class "$5"}'

// What `hl cluster add` prints (cli/accounts.go kubernetesScript). It reads the runtime token,
// without echo when a person types it, and hands it to helm on standard input. $1 is the chart,
// $2 its version, the rest the chart's values.
const KUBERNETES_SCRIPT =
  '[ -t 0 ] && { printf "Runtime token: " >&2; trap "stty echo" EXIT; stty -echo; }; IFS= read -r token; [ -t 0 ] && echo >&2; ' +
  'chart="$1" version="$2"; shift 2; ' +
  'printf %s "$token" | helm upgrade --install hypothesisloop-cluster-agent "$chart" --version "$version" ' +
  '--namespace hypothesisloop --create-namespace --take-ownership --set-file runtimeToken=/dev/stdin "$@"'

// Neither command holds the runtime token, so it is in no shell history. Each asks for it when it
// runs, and it is shown beside the command, so the machine needs neither hl nor an API key.
function startCommand(kind: Kind, cluster: RegisteredCluster): string {
  const name = shellQuote(cluster.name)
  if (kind === 'bare-metal') {
    const image = shellQuote(`${cluster.registry}/hypothesisloop-bare-agent:${cluster.image_tag}`)
    return `sudo sh -c ${shellQuote(BARE_METAL_SCRIPT)} sh ${name} ${shellQuote(cluster.api_url)} ${image} ${shellQuote(cluster.job_image)}`
  }
  const set = (key: string, value: string) => ` --set-string ${shellQuote(`${key}=${helmValue(value)}`)}`
  return `sh -c ${shellQuote(KUBERNETES_SCRIPT)} sh ${shellQuote(`oci://${cluster.registry}/charts/hypothesisloop-cluster-agent`)} ${shellQuote(`0.1.0+${cluster.image_tag}`)}` +
    set('clusterName', cluster.name) + set('apiURL', cluster.api_url) + set('metricsURL', cluster.metrics_url) +
    set('images.registry', cluster.registry) + set('images.tag', cluster.image_tag) + set('jobImage', cluster.job_image)
}

// helm splits a --set-string value on commas and reads a backslash as an escape.
function helmValue(text: string): string {
  return text.replace(/\\/g, '\\\\').replace(/,/g, '\\,')
}

/**
 * Where a cluster the org registered stands among the ones its jobs are placed on. The field
 * holds what is typed until it is set.
 */
function ClusterPriority({ cluster, onSet, disabled }: { cluster: OrgCluster; onSet: (priority: number) => Promise<boolean>; disabled: boolean }) {
  const [draft, setDraft] = useState<string | null>(null)
  const value = draft ?? String(cluster.priority)
  const priority = Number(value)
  const changed = draft !== null && draft.trim() !== '' && Number.isInteger(priority) && priority !== cluster.priority

  async function submit(e: React.FormEvent) {
    e.preventDefault()
    if (changed && await onSet(priority)) setDraft(null)
  }

  return (
    <form className="flex items-center gap-1.5" onSubmit={submit}>
      <Input
        fieldSize="sm"
        mono
        type="number"
        min={1}
        max={1000}
        step={1}
        className="w-20"
        aria-label={`Priority of ${cluster.name}`}
        title="1 to 1000. A job is placed on the highest-priority cluster that can take it."
        value={value}
        onChange={e => setDraft(e.target.value)}
      />
      {changed && <Button size="sm" type="submit" disabled={disabled}>Set</Button>}
    </form>
  )
}

/**
 * The clusters the org's jobs can run on: its own, with the scheduler settings an owner reads and
 * changes, and any the platform provides, in the order a job is placed on them, each with its
 * priority and whether the org uses it for its jobs. With
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

  async function setPriority(cluster: OrgCluster, priority: number): Promise<boolean> {
    setSwitching(cluster.name)
    setActionError('')
    try {
      await setClusterPriority(org.id, cluster.name, priority)
      await mutate()
      return true
    } catch (e) {
      setActionError(e instanceof Error ? e.message : String(e))
      return false
    } finally {
      setSwitching('')
    }
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
          value={startCommand(kind, registered)}
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
              <th>Priority</th>
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
                <td className="text-xs">
                  {manages && c.owned && !c.shared
                    ? <ClusterPriority cluster={c} onSet={priority => setPriority(c, priority)} disabled={switching !== ''} />
                    : <span className="font-mono">{c.priority}</span>}
                  {c.shared && <div className="mt-0.5 text-2xs text-muted">used after {org.name}&apos;s own</div>}
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
