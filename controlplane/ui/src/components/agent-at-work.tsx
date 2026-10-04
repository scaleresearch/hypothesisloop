'use client'

import { clsx } from 'clsx'
import { relTime } from '@/lib/format'
import { useTick } from '@/lib/tick'
import type { AgentActivity, AgentInstance, JoinedAgent } from '@/types'
import { Stack } from '@/components/ui/stack'
import { Steps } from '@/components/ui/steps'
import { Text } from '@/components/ui/text'

const MACHINE_STEPS = [
  { key: 'requested', label: 'Machine asked for' },
  { key: 'starting', label: 'Machine starting' },
  { key: 'running', label: 'Working' },
]

/** What an agent's machine is doing now. One asked for after another ended is a restart, and says so. */
export function machineSentence(instance: AgentInstance | undefined): string {
  if (!instance) return 'Its machine is being asked for.'
  const restart = instance.restarts > 0 ? `restart ${instance.restarts}` : ''
  switch (instance.state) {
    case 'waiting': return 'Its machine is asked for when the run starts.'
    case 'requested': return restart ? `Its machine ended and another is being asked for (${restart}).` : 'Its machine is being asked for.'
    case 'starting': return restart ? `Its machine ended and another is starting (${restart}).` : 'Its machine is starting, which takes about a minute.'
    case 'running': return restart ? `Its machine is working again (${restart}).` : 'Its machine is working.'
    case 'stopped': return instance.end_reason === 'released' ? 'Its work is over and its machine was released.' : 'Its machine stopped and is not asked for again.'
  }
}

/** The sentence an agent last said of what it is doing, with its age, which keeps up with the clock. */
export function Activity({ activity, prominent = false, className }: { activity: AgentActivity; prominent?: boolean; className?: string }) {
  useTick(1_000)
  return (
    <p className={clsx(prominent ? 'text-base font-medium' : 'text-xs', className)}>
      <span aria-live="polite">{activity.text}</span>{' '}
      <time dateTime={activity.at} title={new Date(activity.at).toLocaleString()} className="whitespace-nowrap text-xs font-normal text-subtle">{relTime(activity.at)}</time>
    </p>
  )
}

const PLANNER_STEPS = [
  { key: 'requested', label: 'Finding a machine' },
  { key: 'starting', label: 'Starting the planner' },
  { key: 'running', label: 'Planning' },
]

/** Where a draft's planner stands, in the person's words. Undefined when its own sentence says enough. */
export function plannerSentence(instance: AgentInstance | undefined, saidSomething: boolean): string | undefined {
  if (!instance) return 'Finding a machine for the planner.'
  const restart = instance.restarts > 0 ? ` (restart ${instance.restarts})` : ''
  switch (instance.state) {
    case 'requested': return restart ? `The planner's machine ended. Finding another${restart}.` : 'Finding a machine for the planner.'
    case 'starting': return restart ? `The planner's machine ended. Starting another${restart}.` : 'Starting the planner, usually about a minute.'
    case 'running': return restart ? `The planner is back at work${restart}.` : saidSomething ? undefined : 'The planner is at work.'
    case 'stopped': return instance.end_reason === 'released' ? 'The planner is done.' : 'The planner stopped and is not started again.'
    default: return machineSentence(instance)
  }
}

/** A draft's planner: what it says it is doing, first and largest, then where it stands. */
export function PlannerAtWork({ agent, activity }: { agent: JoinedAgent | undefined; activity?: AgentActivity }) {
  const instance = agent?.instance
  const sentence = agent?.finished_at ? `${agent.name} finished: ${agent.finished_reason}` : plannerSentence(instance, !!activity)
  return (
    <Stack gap="sm">
      {activity && <Activity activity={activity} prominent />}
      <Steps label="Machine" steps={PLANNER_STEPS} current={instance?.state ?? 'requested'} live />
      {sentence && <Text variant="note" aria-live="polite">{sentence}</Text>}
    </Stack>
  )
}

/**
 * The one agent a phase waits for, a draft's planner or the baseline agent: where its machine
 * stands, as its launcher reported it, and what the agent says it is doing. `activity` is the
 * roster's unless the caller read it elsewhere: a draft carries its planner's itself.
 */
export function AgentAtWork({ agent, activity = agent?.activity }: { agent: JoinedAgent | undefined; activity?: AgentActivity }) {
  const instance = agent?.instance
  const at = instance?.state ?? 'requested'
  return (
    <Stack gap="sm">
      <Steps label="Machine" steps={MACHINE_STEPS} current={at} live />
      <Text variant="note" aria-live="polite">
        {agent?.finished_at ? `${agent.name} finished: ${agent.finished_reason}` : machineSentence(instance)}
      </Text>
      {activity && <Activity activity={activity} />}
    </Stack>
  )
}
