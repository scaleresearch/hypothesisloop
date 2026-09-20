'use client'

import { useEffect, useMemo, useRef } from 'react'
import { clsx } from 'clsx'
import type { HypothesisGraph } from '@/types'
import type { HypothesisEvidence } from '@/lib/api'
import { Badge } from '@/components/ui/badge'
import { NodeMeta } from '@/components/hypothesis-graph'
import { matchesFilters, outlineRows } from '@/lib/hypothesis-graph'
import { verdictSetBy } from '@/lib/hypothesis-progress'
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
    <ul className="hg-outline" aria-label="Hypotheses by what they build on">
      <li>
        <button
          ref={selected === graph.root ? selectedRef : undefined}
          className={clsx('hg-outline-row', selected === graph.root && 'selected')}
          aria-current={selected === graph.root}
          onClick={() => onSelect(graph.root)}
        >
          <span className="uppercase-label">Objective</span>
          <span className="hg-outline-text">{rootLabel}</span>
        </button>
      </li>
      {rows.map(({ node, depth, alsoBuildsOn }) => (
        <li key={node.id}>
          <button
            ref={selected === node.id ? selectedRef : undefined}
            className={clsx(
              'hg-outline-row',
              selected === node.id && 'selected',
              hits.has(node.id) && 'hit',
              !matchesFilters(node, filters) && 'dimmed',
            )}
            style={{ paddingLeft: 12 + (depth + 1) * 18 }}
            aria-current={selected === node.id}
            onClick={() => onSelect(node.id)}
          >
            <span title={verdictSetBy(node) && `${node.status} ${verdictSetBy(node)}`}><Badge status={node.status}>{node.status}</Badge></span>
            <span className="hg-outline-text" title={node.text}>{node.text}</span>
            {alsoBuildsOn.length > 0 && (
              <span className="text-dim" style={{ fontSize: 11, whiteSpace: 'nowrap' }}>
                also builds on {alsoBuildsOn.length}
              </span>
            )}
            <NodeMeta item={node} evidence={evidence.get(node.id)} />
          </button>
        </li>
      ))}
    </ul>
  )
}
