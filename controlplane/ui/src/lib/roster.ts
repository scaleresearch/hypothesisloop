import useSWR from 'swr'
import { fetchPlatformExperimentAgents } from '@/lib/api'
import { phaseOf } from '@/lib/phase'
import type { PlatformExperiment } from '@/types'

const WHILE_PLANNING_MS = 3_000
const OTHERWISE_MS = 15_000

/**
 * How often an experiment's page asks the API again: often while its planner works, when what
 * the planner says it is doing is all there is to watch, and at the page's usual pace otherwise.
 */
export function pollEvery(pe: Pick<PlatformExperiment, 'id' | 'status' | 'status_reason'> | undefined): number {
  return pe && phaseOf(pe) === 'planning' ? WHILE_PLANNING_MS : OTHERWISE_MS
}

/**
 * The agents of a platform experiment with what each is doing, the one read of the roster on
 * its page: every part that shows an agent reads this key. SWR asks only while the page is visible.
 */
export function useRoster(pe: Pick<PlatformExperiment, 'id' | 'status' | 'status_reason'>) {
  return useSWR(['pe-agents', pe.id], () => fetchPlatformExperimentAgents(pe.id), { refreshInterval: pollEvery(pe) })
}
