'use client'

import { useState } from 'react'
import useSWR from 'swr'
import { fetchAgentsPage } from '@/lib/api'
import { useTenant } from '@/lib/tenant'
import { SearchableSelect } from '@/components/ui/searchable-select'

const LISTED = 50

/** Picks an agent by id or name; typing searches all of them on the server. */
export function AgentSelect({ value, onChange }: { value: string; onChange: (id: string) => void }) {
  const { project } = useTenant()
  const [q, setQ] = useState('')
  const { data } = useSWR(
    ['agent-search', project.id, q],
    () => fetchAgentsPage({ project_id: project.id, q, limit: LISTED }),
    { keepPreviousData: true },
  )
  const listed = data?.items ?? []
  const more = (data?.total ?? 0) - listed.length
  return (
    <SearchableSelect
      value={value}
      onChange={onChange}
      onSearch={setQ}
      allLabel="All agents"
      options={listed.map(a => ({ value: a.id, label: a.name === a.id ? a.id : `${a.name} (${a.id})` }))}
      hint={more > 0 ? `${more} more, type to search` : undefined}
    />
  )
}
