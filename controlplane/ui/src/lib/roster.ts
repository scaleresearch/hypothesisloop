import useSWR from 'swr'
import { fetchPlatformExperimentAgents } from '@/lib/api/agents'
import type { PlatformExperiment } from '@/types/platform-experiments'
import { keys } from '@/lib/keys'

/**
 * The agents of a platform experiment with what each is doing, the one read of the roster on
 * its page: every part that shows an agent reads this key. It is read again on every change of
 * an agent the stream tells, and not before the experiment itself is read.
 */
export function useRoster(pe: Pick<PlatformExperiment, 'id'> | undefined) {
  return useSWR(pe ? keys.roster(pe.id) : null, () => fetchPlatformExperimentAgents(pe!.id))
}
