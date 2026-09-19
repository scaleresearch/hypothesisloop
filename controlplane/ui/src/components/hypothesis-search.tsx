'use client'

import { useState } from 'react'
import type { CSSProperties } from 'react'
import useSWR from 'swr'
import { searchHypotheses } from '@/lib/api'
import type { HypothesisSearchHit } from '@/lib/api'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Loading } from '@/components/ui/status-message'

export const INPUT_STYLE: CSSProperties = {
  width: '100%', padding: '7px 10px', border: '1px solid var(--border)', borderRadius: 6,
  fontSize: 13, fontFamily: 'inherit', boxSizing: 'border-box',
  background: 'var(--surface-2)', color: 'var(--foreground)',
}

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
      style={{ display: 'flex', gap: 6, flex: 1, minWidth: 200 }}
      onSubmit={e => { e.preventDefault(); onSearch(input.trim()) }}
    >
      <input
        type="search"
        style={INPUT_STYLE}
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
  if (error) return <p className="text-error">Search failed: {error.message}</p>
  if (!hits) return null
  if (hits.length === 0) return <p className="text-dim">No matching hypotheses.</p>
  return (
    <ul className="hg-hits">
      {hits.map(h => (
        <li key={h.id}>
          <button className="hg-hit" onClick={() => onPick(h)} title={h.text}>
            <Badge status={h.status}>{h.status}</Badge>
            <span className="hg-outline-text">{h.text}</span>
          </button>
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
    <div className="hg-stack" style={{ gap: 6 }}>
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
