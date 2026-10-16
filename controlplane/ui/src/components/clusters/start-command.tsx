'use client'

import { useState, type ReactNode } from 'react'
import type { ClusterKind as Kind, RegisteredCluster } from '@/types/clusters'
import { CopyButton } from '@/components/ui/copy-button'
import { Secret } from '@/components/ui/secret'
import { Segmented } from '@/components/ui/segmented'
import { Stack } from '@/components/ui/stack'
import { Code, Heading } from '@/components/ui/text'

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

/** The command that attaches a registered cluster, for the kind chosen in it, and the token it asks for. */
export function StartCommand({ registered, onHide }: { registered: RegisteredCluster; onHide: () => void }) {
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
