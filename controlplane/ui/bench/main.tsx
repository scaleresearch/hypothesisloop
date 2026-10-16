import { useEffect, useMemo, useRef, useState } from 'react'
import { createRoot } from 'react-dom/client'
import '@/app/globals.css'
import { HypothesisGraphCanvas } from '@/components/hypothesis-graph'
import { Expandable } from '@/components/ui/expandable'
import { extendLayout, layoutGraph } from '@/lib/hypothesis-graph'
import type { GraphFilters, Position } from '@/lib/hypothesis-graph'
import type { HypothesisGraph } from '@/types/hypotheses'
import { fixtureGraph } from './fixture'

// The hypotheses page's layout without its tidy step: laid out whole once, extended on each poll.
function useStableLayout(graph: HypothesisGraph) {
  const last = useRef<{ positions: Map<string, Position> } | null>(null)
  const layout = useMemo(() => {
    const prev = last.current
    if (!prev) return { positions: layoutGraph(graph) }
    return { positions: extendLayout(graph, prev.positions) }
  }, [graph])
  useEffect(() => {
    last.current = layout
  }, [layout])
  return layout
}

interface Bench {
  graph: HypothesisGraph
  selected: string
  filters: GraphFilters
}

declare global {
  interface Window {
    bench: {
      set: (change: (b: Bench) => Bench) => void
      fixture: typeof fixtureGraph
    }
  }
}

const NO_HITS = new Set<string>()

function App({ initial }: { initial: Bench }) {
  const [bench, setBench] = useState(initial)
  window.bench = { set: setBench, fixture: fixtureGraph }
  const { positions } = useStableLayout(bench.graph)
  return (
    <Expandable label="Hypothesis graph" className="contents">
    <HypothesisGraphCanvas
      graph={bench.graph}
      positions={positions}
      rootLabel="Bench objective"
      focus=""
      selected={bench.selected}
      hits={NO_HITS}
      filters={bench.filters}
      minimap
      onSelect={id => setBench(b => ({ ...b, selected: id }))}
    />
    </Expandable>
  )
}

const n = Number(new URLSearchParams(location.search).get('n') ?? 200)
const graph = fixtureGraph(n)
createRoot(document.getElementById('root')!).render(
  <App initial={{ graph, selected: graph.root, filters: { status: '', owner: '' } }} />,
)
