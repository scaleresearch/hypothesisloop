'use client'

import type { AgentInstance } from '@/types/agents'
import type { Tone } from '@/lib/colors'
import { relTime } from '@/lib/format'
import { useTick } from '@/lib/tick'
import { Badge } from '@/components/ui/badge'
import { CodeBlock } from '@/components/ui/code-block'
import { Disclosure } from '@/components/ui/disclosure'
import { Inline, Stack } from '@/components/ui/stack'
import { Text } from '@/components/ui/text'

const STATE_TONE: Record<AgentInstance['state'], Tone> = {
  waiting: 'neutral',
  requested: 'neutral',
  starting: 'warning',
  running: 'success',
  stopped: 'danger',
}

// Why the last machine that ended was given up on, to follow "Its last machine ended".
const END_REASON: Record<NonNullable<AgentInstance['end_reason']>, string> = {
  exited: 'its command ended',
  gone: 'its provider no longer lists it',
  never_seen: 'no machine came to be',
  still_starting: 'it did not come up in time',
  silent: 'it ran and made no progress',
  expired: 'its provider ended it at the provider’s own lifetime, which counts as no restart',
  progress_unreadable: 'its output could not be read for as long as progress is waited for, which counts as no restart',
  released: 'the platform took it down',
}

/**
 * The health of the machine the platform runs for a hosted agent or a coordinator: what it is
 * doing now, since when, how often it was restarted, and what became of the last
 * machine that ended, with the end of its log folded away.
 */
export function InstanceHealth({ instance }: { instance: AgentInstance }) {
  useTick(15_000)
  const ended = instance.end_reason
  const released = ended === 'released'
  const how = released ? 'Released: its run was over and its machine ended'
    : instance.stopped ? 'Stopped for good: its last machine ended'
    : 'Replaced: the machine before this one ended'
  return (
    <Stack gap="xs" start>
      <Inline>
        <Badge tone={released && instance.state === 'stopped' ? 'neutral' : STATE_TONE[instance.state]}>{instance.state}</Badge>
        {instance.state !== 'stopped' && instance.state !== 'waiting' && (
          <Text as="span" variant="note">
            {instance.seen_at ? `first reported ${relTime(instance.seen_at)}` : `asked for ${relTime(instance.asked_at)}`}
          </Text>
        )}
        {instance.restarts > 0 && (
          <Text as="span" variant="hint">restarted {instance.restarts === 1 ? 'once' : `${instance.restarts} times`}</Text>
        )}
      </Inline>
      {ended && (
        <Text variant="hint">
          {how}
          {instance.exited_at ? ` ${relTime(instance.exited_at)}` : ''}, {END_REASON[ended] ?? ended}.
        </Text>
      )}
      {instance.exit_log && released && <Text variant="hint">{instance.exit_log}</Text>}
      {instance.exit_log && !released && (
        <Disclosure label="Exit log">
          <CodeBlock framed height="sm" className="max-w-xl">{instance.exit_log}</CodeBlock>
        </Disclosure>
      )}
    </Stack>
  )
}
