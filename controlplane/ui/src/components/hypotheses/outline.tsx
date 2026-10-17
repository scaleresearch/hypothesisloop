'use client'

import { memo, useEffect, useMemo, useRef } from 'react'
import type { ReactNode, RefObject } from 'react'
import { clsx } from 'clsx'
import { Badge } from '@/components/ui/badge'
import { RowButton } from '@/components/ui/button'
import { Inline } from '@/components/ui/stack'
import { Eyebrow, Text } from '@/components/ui/text'
import { NodeMeta, ObjectiveMeta } from '@/components/hypotheses/node-meta'
import { useEditor } from '@/components/hypotheses/editor'
import { AddHereButton } from '@/components/hypotheses/editor-controls'
import { matchesFilters, outlineRows } from '@/lib/hypothesis-graph'
import { verdictBy } from '@/components/actor'
import type { GraphFilters } from '@/lib/hypothesis-graph'
import type { GraphNode, HypothesisGraph } from '@/types/api'

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
      <Inline nowrap className="group pr-1.5">
        <RowButton
          ref={selected ? selectedRef : undefined}
          selected={selected}
          hit={hit}
          className={clsx(className, picking && (pickable ? 'cursor-crosshair' : 'cursor-not-allowed opacity-dimmed'))}
          style={{ paddingLeft: indent(depth) }}
          aria-current={selected}
          onClick={() => (picking ? pickable && editor!.pick(id) : onSelect(id))}
        >
          {children}
        </RowButton>
        <AddHereButton id={id} />
      </Inline>
    </li>
  )
}

// One row per idea, memoized on its own props: selecting, filtering or a poll that changes one idea
// re-renders the rows that changed and not the thousand around them.
const IdeaRow = memo(function IdeaRow({ node, depth, alsoBuildsOn, selected, hit, dimmed, selectedRef, onSelect }: {
  node: GraphNode
  depth: number
  alsoBuildsOn: number
  selected: boolean
  hit: boolean
  dimmed: boolean
  selectedRef: RefObject<HTMLButtonElement>
  onSelect: (id: string) => void
}) {
  return (
    <Row id={node.id} depth={depth} selected={selected} hit={hit} className={clsx(dimmed && 'opacity-dimmed')} selectedRef={selectedRef} onSelect={onSelect}>
      <span title={verdictBy(node) && `${node.status} ${verdictBy(node)}`}><Badge status={node.status}>{node.status}</Badge></span>
      <span className="min-w-0 flex-1 truncate" title={node.text}>{node.text}</span>
      {alsoBuildsOn > 0 && (
        <Text as="span" variant="meta" tone="subtle" className="whitespace-nowrap">
          also refines {alsoBuildsOn}
        </Text>
      )}
      <NodeMeta item={node} />
    </Row>
  )
})

/** The graph as an indented builds_on tree: scannable, and reachable by keyboard and screen reader. */
export function HypothesisOutline({ graph, rootLabel, selected, hits, filters, onSelect }: HypothesisOutlineProps) {
  const rows = useMemo(() => outlineRows(graph), [graph])
  const selectedRef = useRef<HTMLButtonElement>(null)

  useEffect(() => {
    selectedRef.current?.scrollIntoView({ block: 'nearest' })
  }, [selected])

  const common = { selectedRef, onSelect }
  return (
    <ul className="h-full list-none overflow-y-auto p-1.5" aria-label="Hypotheses by what they refine">
      <Row id={graph.root} depth={0} selected={selected === graph.root} hit={false} {...common}>
        <Eyebrow>Objective</Eyebrow>
        <span className="min-w-0 flex-1 truncate">{rootLabel}</span>
        <ObjectiveMeta objective={graph.objective} />
      </Row>
      {rows.map(({ node, depth, alsoBuildsOn }) => (
        <IdeaRow
          key={node.id}
          node={node}
          depth={depth + 1}
          alsoBuildsOn={alsoBuildsOn.length}
          selected={selected === node.id}
          hit={hits.has(node.id)}
          dimmed={!matchesFilters(node, filters)}
          {...common}
        />
      ))}
    </ul>
  )
}
