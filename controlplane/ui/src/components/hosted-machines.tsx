'use client'

import { relTime } from '@/lib/format'
import { useRoster } from '@/lib/roster'
import { PlatformExperimentStatus } from '@/types'
import type { AgentInstance, PlatformExperiment } from '@/types'
import { Text } from '@/components/ui/text'

const plural = (n: number, one: string) => `${n} ${one}${n === 1 ? '' : 's'}`

/**
 * What the machines of a hosted experiment are doing, in one sentence and what comes next. The
 * first minutes after an experiment starts show no hypothesis, no job and no metric, and this
 * is what says that something is happening.
 */
function machinesStatus(instances: AgentInstance[], jobs: number): { now: string; next: string } | null {
  if (instances.length === 0) return null
  const count = (state: AgentInstance['state']) => instances.filter(i => i.state === state).length
  const parts = [
    count('running') && `${count('running')} running`,
    count('starting') && `${count('starting')} starting`,
    count('requested') && `${count('requested')} asked for`,
    count('stopped') && `${count('stopped')} stopped`,
  ].filter(Boolean)
  const now = `The platform runs ${plural(instances.length, 'machine')} for this experiment: ${parts.join(', ')}.`
  const latest = instances.map(i => i.progress_at).filter((at): at is string => !!at).sort().pop()
  if (count('stopped') === instances.length) return { now, next: 'None is asked for again. Agents, below, says why each ended.' }
  if (!latest) return { now, next: 'A machine boots and clones the repository in about a minute.' }
  const progress = `Last progress ${relTime(latest)}.`
  if (jobs === 0) return { now, next: `${progress} The agents are reading the brief and the code. Their first hypotheses and jobs follow within a few minutes.` }
  return { now, next: progress }
}

/** The status of the machines of a running experiment, under its title. Nothing for an experiment with none. */
export function HostedMachines({ pe, jobs }: { pe: PlatformExperiment; jobs: number }) {
  const { data: agents } = useRoster(pe)
  if (pe.status !== PlatformExperimentStatus.RUNNING || !agents) return null
  const instances = [pe.coordinator?.instance, ...agents.map(a => a.instance)].filter((i): i is AgentInstance => !!i)
  const status = machinesStatus(instances, jobs)
  if (!status) return null
  return (
    <Text variant="note" role="status">
      {status.now} {status.next}
    </Text>
  )
}
