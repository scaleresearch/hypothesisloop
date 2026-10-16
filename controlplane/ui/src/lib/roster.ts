import useSWR from 'swr'
import { fetchPlatformExperimentAgents } from '@/lib/api/agents'
import type { PlatformExperiment } from '@/types/platform-experiments'
import { phaseOf } from '@/lib/phase'
import { keys } from '@/lib/keys'
import { POLL } from '@/lib/poll'

/**
 * How often an experiment's page asks the API again: often while its planner works, when what
 * the planner says it is doing is all there is to watch, and while its proposal counts down to
 * being accepted, and at the page's usual pace otherwise.
 */
export function pollEvery(pe: Pick<PlatformExperiment, 'id' | 'status' | 'status_reason'> | undefined): number {
  return pe && (phaseOf(pe) === 'planning' || pe.status_reason?.reason === 'accepting') ? POLL.planning : POLL.standing
}

/**
 * The agents of a platform experiment with what each is doing, the one read of the roster on
 * its page: every part that shows an agent reads this key. SWR asks only while the page is visible,
 * and not before the experiment itself is read.
 */
export function useRoster(pe: Pick<PlatformExperiment, 'id' | 'status' | 'status_reason'> | undefined) {
  return useSWR(pe ? keys.roster(pe.id) : null, () => fetchPlatformExperimentAgents(pe!.id), { refreshInterval: pollEvery(pe) })
}
