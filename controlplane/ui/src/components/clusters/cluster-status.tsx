import { Badge, StatusLabel } from '@/components/ui/badge'
import { Stack } from '@/components/ui/stack'
import { Code, Text } from '@/components/ui/text'
import { formatDate, relTime } from '@/lib/format'
import type { ListedCluster } from '@/types/api'

/**
 * Whether the cluster's agent is connected, when it was last heard from, the version it runs and
 * whether that is the platform's own, and what the platform last refused it with.
 */
export function ClusterStatus({ cluster: c }: { cluster: ListedCluster }) {
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
