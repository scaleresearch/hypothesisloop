'use client'

import { useState } from 'react'
import useSWR from 'swr'
import { searchHypotheses } from '@/lib/api/hypotheses'
import type { HypothesisSearchHit } from '@/types/hypotheses'
import { Badge } from '@/components/ui/badge'
import { Button, RowButton } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Inline, Stack } from '@/components/ui/stack'
import { InlineError, Loading } from '@/components/ui/status-message'
import { Text } from '@/components/ui/text'
import { keys } from '@/lib/keys'

export function useHypothesisSearch(platformExperimentID: string, q: string) {
  return useSWR(
    platformExperimentID && q ? keys.hypothesisSearch(platformExperimentID, q) : null,
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
    <Inline
      as="form"
      role="search"
      gap="xs"
      nowrap
      className="min-w-52 flex-1"
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
    </Inline>
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
  if (hits.length === 0) return <Text variant="hint">No matching hypotheses.</Text>
  return (
    <Stack as="ul" gap="2xs" className="list-none">
      {hits.map(h => (
        <li key={h.id}>
          <RowButton className="text-xs" onClick={() => onPick(h)} title={h.text}>
            <Badge status={h.status}>{h.status}</Badge>
            <span className="min-w-0 flex-1 truncate">{h.text}</span>
          </RowButton>
        </li>
      ))}
    </Stack>
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
    <Stack gap="sm">
      <SearchBox initial="" placeholder="Search hypotheses by text" onSearch={setQ} />
      <SearchHits
        hits={data?.filter(h => !exclude.includes(h.id))}
        onPick={onPick}
        isLoading={isLoading}
        error={error}
      />
    </Stack>
  )
}
