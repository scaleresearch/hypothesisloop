'use client'

import { useRoster } from '@/lib/roster'
import { Text } from '@/components/ui/text'
import type { AgentInstance, PlatformExperiment } from '@/types/api'

const plural = (n: number, one: string) => `${n} ${one}${n === 1 ? '' : 's'}`

/**
 * What the machines of a hosted experiment are doing, in one sentence and what comes next. The
 * first minutes after an experiment starts show no hypothesis, no job and no metric, and this
 * is what says that something is happening.
 */
function machinesStatus(instances: AgentInstance[], jobs: number): string | null {
  if (instances.length === 0) return null
  const count = (state: AgentInstance['state']) => instances.filter(i => i.state === state).length
  const parts = [
    count('running') && `${count('running')} running`,
    count('starting') && `${count('starting')} starting`,
    count('requested') && `${count('requested')} asked for`,
    count('stopped') && `${count('stopped')} stopped`,
  ].filter(Boolean)
  const now = `The platform runs ${plural(instances.length, 'machine')} for this experiment: ${parts.join(', ')}.`
  if (count('stopped') === instances.length) return `${now} None is asked for again. Agents, below, says why each ended.`
  if (count('running') === 0) return `${now} A machine boots and clones the repository in about a minute.`
  if (jobs === 0) return `${now} The agents are reading the brief and the code. Their first hypotheses and jobs follow within a few minutes.`
  return now
}

/** The status of the machines of a running experiment, under its title. Nothing for an experiment with none. */
export function HostedMachines({ pe, jobs }: { pe: PlatformExperiment; jobs: number }) {
  const { data: agents } = useRoster(pe)
  if (pe.status !== 'running' || !agents) return null
  const instances = [pe.coordinator?.instance, ...agents.map(a => a.instance)].filter((i): i is AgentInstance => !!i)
  const status = machinesStatus(instances, jobs)
  if (!status) return null
  return (
    <Text variant="note" role="status">
      {status}
    </Text>
  )
}
