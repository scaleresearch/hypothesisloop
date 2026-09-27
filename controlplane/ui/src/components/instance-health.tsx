'use client'

import { useEffect, useState } from 'react'
import type { AgentInstance } from '@/types'
import type { Tone } from '@/lib/colors'
import { relTime } from '@/lib/format'
import { Badge } from '@/components/ui/badge'
import { Disclosure } from '@/components/ui/disclosure'

const STATE_TONE: Record<AgentInstance['state'], Tone> = {
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
  released: 'the platform took it down',
}

// Rerenders on a timer, so an age on screen keeps up with the clock between reads of the API.
function useTick(everyMs: number) {
  const [, tick] = useState(0)
  useEffect(() => {
    const timer = setInterval(() => tick(n => n + 1), everyMs)
    return () => clearInterval(timer)
  }, [everyMs])
}

/**
 * The health of the machine the platform runs for a hosted agent or a coordinator: what it is
 * doing now, when it last made progress, how often it was restarted, and what became of the last
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
    <div className="grid justify-items-start gap-1">
      <div className="flex flex-wrap items-center gap-x-2.5 gap-y-1">
        <Badge tone={released && instance.state === 'stopped' ? 'neutral' : STATE_TONE[instance.state]}>{instance.state}</Badge>
        {instance.state !== 'stopped' && (
          <span className="text-xs text-muted">
            {instance.progress_at ? `last progress ${relTime(instance.progress_at)}` : 'no progress seen yet'}
          </span>
        )}
        {instance.restarts > 0 && (
          <span className="text-xs text-subtle">restarted {instance.restarts === 1 ? 'once' : `${instance.restarts} times`}</span>
        )}
      </div>
      {ended && (
        <p className="text-xs text-subtle">
          {how}
          {instance.exited_at ? ` ${relTime(instance.exited_at)}` : ''}, {END_REASON[ended] ?? ended}.
        </p>
      )}
      {instance.exit_log && released && <p className="text-xs text-subtle">{instance.exit_log}</p>}
      {instance.exit_log && !released && (
        <Disclosure label="Exit log">
          <pre className="max-h-48 max-w-[560px] overflow-auto whitespace-pre-wrap break-words rounded border bg-surface-inset px-2.5 py-2 font-mono text-2xs text-muted">{instance.exit_log}</pre>
        </Disclosure>
      )}
    </div>
  )
}
