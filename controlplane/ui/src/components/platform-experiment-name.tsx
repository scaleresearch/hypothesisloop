'use client'

import useSWR from 'swr'
import { fetchPlatformExperiment } from '@/lib/api/platform-experiments'
import { keys } from '@/lib/keys'

/** A platform experiment's name for a row that holds only its id, read per id so no list page bounds it. */
export function PlatformExperimentName({ id }: { id: string }) {
  const { data } = useSWR(keys.experiment(id), () => fetchPlatformExperiment(id))
  return <>{data?.name ?? id}</>
}
