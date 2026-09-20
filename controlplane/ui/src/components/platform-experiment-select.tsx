'use client'

import { useState } from 'react'
import useSWR from 'swr'
import { fetchPlatformExperiment, fetchPlatformExperimentsPage } from '@/lib/api'
import { useTenant } from '@/lib/tenant'
import { SearchableSelect } from '@/components/ui/searchable-select'

const LISTED = 50

/** Picks a platform experiment by name; typing searches all of them on the server. */
export function PlatformExperimentSelect({ value, onChange, allLabel, placeholder, hideAllOption, className }: {
  value: string
  onChange: (id: string) => void
  allLabel?: string
  placeholder?: string
  hideAllOption?: boolean
  className?: string
}) {
  const { project } = useTenant()
  const [q, setQ] = useState('')
  const { data } = useSWR(
    ['platform-experiment-search', project.id, q],
    () => fetchPlatformExperimentsPage({ project_id: project.id, q, sort: '-starts_at', limit: LISTED }),
    { keepPreviousData: true },
  )
  const { data: selected } = useSWR(value ? ['pe', value] : null, () => fetchPlatformExperiment(value))

  const listed = data?.items ?? []
  const more = (data?.total ?? 0) - listed.length
  return (
    <SearchableSelect
      value={value}
      onChange={onChange}
      onSearch={setQ}
      allLabel={allLabel}
      placeholder={placeholder}
      hideAllOption={hideAllOption}
      className={className}
      options={(selected && !listed.some(p => p.id === selected.id) ? [selected, ...listed] : listed)
        .map(p => ({ value: p.id, label: `${p.name} (${p.status})` }))}
      hint={more > 0 ? `${more} more, type to search` : undefined}
    />
  )
}
