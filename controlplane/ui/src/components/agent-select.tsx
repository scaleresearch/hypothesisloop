'use client'

import { useState } from 'react'
import useSWR from 'swr'
import { fetchAgentsPage } from '@/lib/api'
import { useTenant } from '@/lib/tenant'
import { SearchableSelect } from '@/components/ui/searchable-select'

const LISTED = 50

/**
 * Picks an agent by id or name; typing searches all of them on the server. The agents are the
 * selected project's, or with `platformExperimentID` those of that platform experiment alone,
 * for a shared one that lies in no project of the caller's.
 */
export function AgentSelect({ value, onChange, platformExperimentID }: {
  value: string
  onChange: (id: string) => void
  platformExperimentID?: string
}) {
  const { project } = useTenant()
  const [q, setQ] = useState('')
  const scope = platformExperimentID ? { platform_experiment_id: platformExperimentID } : { project_id: project.id }
  const { data } = useSWR(
    ['agent-search', scope, q],
    () => fetchAgentsPage({ ...scope, q, limit: LISTED }),
    { keepPreviousData: true },
  )
  const listed = data?.items ?? []
  const more = (data?.total ?? 0) - listed.length
  return (
    <SearchableSelect
      value={value}
      onChange={onChange}
      onSearch={setQ}
      mono
      allLabel="All agents"
      options={listed.map(a => ({ value: a.id, label: a.name === a.id ? a.id : `${a.name} (${a.id})` }))}
      hint={more > 0 ? `${more} more, type to search` : undefined}
    />
  )
}
