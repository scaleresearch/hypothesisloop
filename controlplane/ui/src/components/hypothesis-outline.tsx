'use client'

import { useEffect, useMemo, useRef } from 'react'
import type { ReactNode, RefObject } from 'react'
import { clsx } from 'clsx'
import type { HypothesisGraph } from '@/types'
import { Badge } from '@/components/ui/badge'
import { RowButton } from '@/components/ui/button'
import { Inline } from '@/components/ui/stack'
import { Eyebrow, Text } from '@/components/ui/text'
import { NodeMeta, ObjectiveMeta } from '@/components/hypothesis-graph'
import { IdeaEditor, NodeActions, useEditor } from '@/components/graph-edit'
import { matchesFilters, outlineRows } from '@/lib/hypothesis-graph'
import { verdictBy } from '@/components/actor'
import type { GraphFilters } from '@/lib/hypothesis-graph'

interface HypothesisOutlineProps {
  graph: HypothesisGraph
  rootLabel: string
  selected: string
  hits: Set<string>
  filters: GraphFilters
  onSelect: (id: string) => void
}

const indent = (depth: number) => 12 + depth * 18

function Row({ id, depth, selected, selectedRef, hit, className, onSelect, children }: {
  id: string
  depth: number
  selected: boolean
  selectedRef: RefObject<HTMLButtonElement>
  hit: boolean
  className?: string
  onSelect: (id: string) => void
  children: ReactNode
}) {
  const editor = useEditor()
  const picking = editor?.picking ?? false
  const pickable = picking && editor!.canPick(id)
  return (
    <li>
      <Inline nowrap>
        <RowButton
          ref={selected ? selectedRef : undefined}
          selected={selected || editor?.adding === id}
          hit={hit}
          className={clsx(className, picking && (pickable ? 'cursor-crosshair' : 'cursor-not-allowed opacity-dimmed'))}
          style={{ paddingLeft: indent(depth) }}
          aria-current={selected}
          onClick={() => (picking ? pickable && editor!.pick(id) : onSelect(id))}
        >
          {children}
        </RowButton>
        {selected && <NodeActions id={id} />}
      </Inline>
      {editor?.adding === id && (
        <div className="py-1.5" style={{ paddingLeft: indent(depth + 1) }}>
          <IdeaEditor />
        </div>
      )}
    </li>
  )
}

/** The graph as an indented builds_on tree: scannable, and reachable by keyboard and screen reader. */
export function HypothesisOutline({ graph, rootLabel, selected, hits, filters, onSelect }: HypothesisOutlineProps) {
  const rows = useMemo(() => outlineRows(graph), [graph])
  const selectedRef = useRef<HTMLButtonElement>(null)

  useEffect(() => {
    selectedRef.current?.scrollIntoView({ block: 'nearest' })
  }, [selected])

  const common = { selectedRef, onSelect }
  return (
    <ul className="h-full list-none overflow-y-auto p-1.5" aria-label="Hypotheses by what they build on">
      <Row id={graph.root} depth={0} selected={selected === graph.root} hit={false} {...common}>
        <Eyebrow>Objective</Eyebrow>
        <span className="min-w-0 flex-1 truncate">{rootLabel}</span>
        <ObjectiveMeta objective={graph.objective} />
      </Row>
      {rows.map(({ node, depth, alsoBuildsOn }) => (
        <Row
          key={node.id}
          id={node.id}
          depth={depth + 1}
          selected={selected === node.id}
          hit={hits.has(node.id)}
          className={clsx(!matchesFilters(node, filters) && 'opacity-dimmed')}
          {...common}
        >
          <span title={verdictBy(node) && `${node.status} ${verdictBy(node)}`}><Badge status={node.status}>{node.status}</Badge></span>
          <span className="min-w-0 flex-1 truncate" title={node.text}>{node.text}</span>
          {alsoBuildsOn.length > 0 && (
            <Text as="span" variant="meta" tone="subtle" className="whitespace-nowrap">
              also builds on {alsoBuildsOn.length}
            </Text>
          )}
          <NodeMeta item={node} />
        </Row>
      ))}
    </ul>
  )
}
