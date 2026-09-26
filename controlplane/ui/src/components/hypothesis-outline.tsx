'use client'

import { useEffect, useMemo, useRef } from 'react'
import { clsx } from 'clsx'
import type { HypothesisGraph } from '@/types'
import type { HypothesisEvidence } from '@/lib/api'
import { Badge } from '@/components/ui/badge'
import { RowButton } from '@/components/ui/button'
import { Eyebrow } from '@/components/ui/text'
import { NodeMeta } from '@/components/hypothesis-graph'
import { matchesFilters, outlineRows } from '@/lib/hypothesis-graph'
import { verdictBy } from '@/components/actor'
import type { GraphFilters } from '@/lib/hypothesis-graph'

interface HypothesisOutlineProps {
  graph: HypothesisGraph
  rootLabel: string
  evidence: Map<string, HypothesisEvidence>
  selected: string
  hits: Set<string>
  filters: GraphFilters
  onSelect: (id: string) => void
}

/** The graph as an indented builds_on tree: scannable, and reachable by keyboard and screen reader. */
export function HypothesisOutline({ graph, rootLabel, evidence, selected, hits, filters, onSelect }: HypothesisOutlineProps) {
  const rows = useMemo(() => outlineRows(graph), [graph])
  const selectedRef = useRef<HTMLButtonElement>(null)

  useEffect(() => {
    selectedRef.current?.scrollIntoView({ block: 'nearest' })
  }, [selected])

  return (
    <ul className="h-full list-none overflow-y-auto p-1.5" aria-label="Hypotheses by what they build on">
      <li>
        <RowButton
          ref={selected === graph.root ? selectedRef : undefined}
          selected={selected === graph.root}
          className="pl-3"
          aria-current={selected === graph.root}
          onClick={() => onSelect(graph.root)}
        >
          <Eyebrow>Objective</Eyebrow>
          <span className="min-w-0 flex-1 truncate">{rootLabel}</span>
        </RowButton>
      </li>
      {rows.map(({ node, depth, alsoBuildsOn }) => (
        <li key={node.id}>
          <RowButton
            ref={selected === node.id ? selectedRef : undefined}
            selected={selected === node.id}
            hit={hits.has(node.id)}
            className={clsx(!matchesFilters(node, filters) && 'opacity-40')}
            style={{ paddingLeft: 12 + (depth + 1) * 18 }}
            aria-current={selected === node.id}
            onClick={() => onSelect(node.id)}
          >
            <span title={verdictBy(node) && `${node.status} ${verdictBy(node)}`}><Badge status={node.status}>{node.status}</Badge></span>
            <span className="min-w-0 flex-1 truncate" title={node.text}>{node.text}</span>
            {alsoBuildsOn.length > 0 && (
              <span className="whitespace-nowrap text-2xs text-subtle">
                also builds on {alsoBuildsOn.length}
              </span>
            )}
            <NodeMeta item={node} evidence={evidence.get(node.id)} />
          </RowButton>
        </li>
      ))}
    </ul>
  )
}
