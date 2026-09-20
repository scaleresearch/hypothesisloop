'use client'

import { useState } from 'react'
import useSWR from 'swr'
import { searchHypotheses } from '@/lib/api'
import type { HypothesisSearchHit } from '@/lib/api'
import { Badge } from '@/components/ui/badge'
import { Button, RowButton } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { InlineError, Loading } from '@/components/ui/status-message'

export function useHypothesisSearch(platformExperimentID: string, q: string) {
  return useSWR(
    platformExperimentID && q ? ['hypothesis-search', platformExperimentID, q] : null,
    () => searchHypotheses(platformExperimentID, q),
  )
}

/** A search box that only queries on submit, so typing never fires a request per keystroke. */
export function SearchBox({ initial, placeholder, onSearch }: {
  initial: string
  placeholder: string
  onSearch: (q: string) => void
}) {
  const [input, setInput] = useState(initial)
  return (
    <form
      role="search"
      className="flex min-w-[200px] flex-1 gap-1.5"
      onSubmit={e => { e.preventDefault(); onSearch(input.trim()) }}
    >
      <Input
        type="search"
        className="w-full"
        value={input}
        onChange={e => setInput(e.target.value)}
        placeholder={placeholder}
        aria-label={placeholder}
      />
      <Button size="sm" type="submit">Search</Button>
    </form>
  )
}

export function SearchHits({ hits, onPick, isLoading, error }: {
  hits: HypothesisSearchHit[] | undefined
  onPick: (hit: HypothesisSearchHit) => void
  isLoading: boolean
  error: Error | undefined
}) {
  if (isLoading) return <Loading rows={2} />
  if (error) return <InlineError>Search failed: {error.message}</InlineError>
  if (!hits) return null
  if (hits.length === 0) return <p className="text-xs text-subtle">No matching hypotheses.</p>
  return (
    <ul className="grid list-none grid-cols-[minmax(0,1fr)] gap-0.5">
      {hits.map(h => (
        <li key={h.id}>
          <RowButton className="text-xs" onClick={() => onPick(h)} title={h.text}>
            <Badge status={h.status}>{h.status}</Badge>
            <span className="min-w-0 flex-1 truncate">{h.text}</span>
          </RowButton>
        </li>
      ))}
    </ul>
  )
}

/** Finds a hypothesis in one platform experiment by text and hands the chosen hit back. */
export function HypothesisPicker({ platformExperimentID, exclude, onPick }: {
  platformExperimentID: string
  exclude: string[]
  onPick: (hit: HypothesisSearchHit) => void
}) {
  const [q, setQ] = useState('')
  const { data, error, isLoading } = useHypothesisSearch(platformExperimentID, q)
  return (
    <div className="grid grid-cols-[minmax(0,1fr)] gap-1.5">
      <SearchBox initial="" placeholder="Search hypotheses by text" onSearch={setQ} />
      <SearchHits
        hits={data?.filter(h => !exclude.includes(h.id))}
        onPick={onPick}
        isLoading={isLoading}
        error={error}
      />
    </div>
  )
}
