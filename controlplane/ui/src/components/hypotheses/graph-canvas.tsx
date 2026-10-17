'use client'

import { useEffect, useMemo, useRef, useState } from 'react'
import { ReactFlow, ReactFlowProvider, Background, ConnectionMode, Controls, MiniMap, Panel, useReactFlow, useStore } from '@xyflow/react'
import type { Edge, Node } from '@xyflow/react'
import '@xyflow/react/dist/style.css'
import { Button } from '@/components/ui/button'
import { Dot } from '@/components/ui/dot'
import { ExpandButton, useExpanded } from '@/components/ui/expandable'
import { Inline } from '@/components/ui/stack'
import { chart, hypothesisLinkStyles, semantic, statusColor, tint } from '@/lib/colors'
import { LINK_KIND_LABELS, NODE_HEIGHT, NODE_WIDTH, ROW, matchesFilters } from '@/lib/hypothesis-graph'
import type { GraphFilters, Position } from '@/lib/hypothesis-graph'
import { opacity as scale } from '@/lib/scale'
import { useEditor } from '@/components/hypotheses/editor'
import { Highlight, NODE_HANDLES, dataOf, isFar, nodeTypes } from '@/components/hypotheses/graph-nodes'
import type { HypothesisNodeData } from '@/components/hypotheses/graph-nodes'
import { hypothesisStatusValues, type HypothesisGraph, type LinkType } from '@/types/api'

// Kinds read apart by line as well as colour: a related link is thin, the others thick. No line is
// dashed: a dash pattern on a long path is rasterized dash by dash, which at hundreds of links
// stalls every frame of a pan or zoom.
const LINK_WIDTH: Record<LinkType, number> = { builds_on: 1.5, related: 1, contradicts: 2.5 }

// A small graph is seen whole on the canvas; the map only takes room from it.
const MINIMAP_FROM = 15

function Legend({ onTidy }: { onTidy?: () => void }) {
  return (
    <Inline gap="md" className="border-b px-3 py-1.5 text-2xs text-muted">
      {(Object.keys(hypothesisLinkStyles) as Array<keyof typeof hypothesisLinkStyles>).map(type => {
        const s = hypothesisLinkStyles[type]
        return (
          <span key={type} className="inline-flex items-center gap-1.5">
            <svg width="26" height="8" aria-hidden="true">
              <line x1="0" y1="4" x2="26" y2="4" stroke={s.color} strokeWidth={LINK_WIDTH[type]} />
            </svg>
            {LINK_KIND_LABELS[type]}
          </span>
        )
      })}
      {hypothesisStatusValues.map(status => (
        <span key={status} className="inline-flex items-center gap-1.5">
          <Dot color={statusColor(status)} />
          {status}
        </span>
      ))}
      <Inline gap="sm" nowrap className="ml-auto">
        {onTidy && (
          <Button variant="link" size="sm" title="New ideas were placed beside the others; lay the whole graph out again" onClick={onTidy}>
            Tidy layout
          </Button>
        )}
        <ExpandButton />
      </Inline>
    </Inline>
  )
}

/** Content pinned to a point of the graph, drawn at screen size whatever the zoom. */
interface HypothesisGraphCanvasProps {
  graph: HypothesisGraph
  positions: Map<string, Position>
  rootLabel: string
  /** The id named in the URL, centred on whenever it changes, so the side panel opening never hides it. */
  focus: string
  selected: string
  hits: Set<string>
  filters: GraphFilters
  /** Off while the side panel narrows the canvas: the map would lie over the cards around the focus. */
  minimap: boolean
  onSelect: (id: string) => void
  /** Present while ideas added since the graph was laid out sit beside the others. */
  onTidy?: () => void
}

export function HypothesisGraphCanvas(props: HypothesisGraphCanvasProps) {
  return (
    <ReactFlowProvider>
      <Canvas {...props} />
    </ReactFlowProvider>
  )
}

function Canvas({ graph, positions, rootLabel, focus, selected, hits, filters, minimap, onSelect, onTidy }: HypothesisGraphCanvasProps) {
  const { setCenter, setViewport, getZoom } = useReactFlow()
  const editor = useEditor()
  const picking = editor?.picking ?? false
  // The "+" of each node, while one can be added; the nodes stay memoized as the editor changes.
  const onAdd = editor?.canWrite && !picking ? editor.startAdding : null
  const far = useStore(isFar)
  const width = useStore(s => s.width)
  const height = useStore(s => s.height)
  const [ready, setReady] = useState(false)
  const centered = useRef('')
  const expanded = useExpanded()
  const flow = useRef<HTMLDivElement>(null)
  const setZoom = (zoom: number) => flow.current?.style.setProperty('--zoom', String(zoom))

  const dimmed = useMemo(
    () => new Set(graph.nodes.filter(n => !matchesFilters(n, filters)).map(n => n.id)),
    [graph, filters],
  )

  const nodes = useMemo<Node[]>(() => [
    {
      id: graph.root,
      type: 'objective',
      position: positions.get(graph.root)!,
      width: NODE_WIDTH,
      height: NODE_HEIGHT,
      handles: NODE_HANDLES,
      data: { label: rootLabel, objective: graph.objective },
    },
    ...graph.nodes.filter(item => positions.has(item.id)).map(item => ({
      id: item.id,
      type: 'hypothesis',
      position: positions.get(item.id)!,
      width: NODE_WIDTH,
      height: NODE_HEIGHT,
      handles: NODE_HANDLES,
      data: dataOf(item),
    })),
  ], [graph, positions, rootLabel])
  const canPick = editor?.canPick
  const pickable = useMemo(
    () => (picking && canPick ? new Set([graph.root, ...graph.nodes.map(n => n.id)].filter(canPick)) : null),
    [picking, canPick, graph],
  )
  const highlight = useMemo(
    () => ({ selected, hits, dimmed, pickable, onAdd }),
    [selected, hits, dimmed, pickable, onAdd],
  )

  const edges = useMemo<Edge[]>(() => {
    const builtOn = new Set(graph.edges.filter(e => e.type === 'builds_on').flatMap(e => [`${e.from}>${e.to}`, `${e.to}>${e.from}`]))
    return graph.edges.filter(e => positions.has(e.from) && positions.has(e.to)).map(e => {
    const s = hypothesisLinkStyles[e.type]
    const hierarchy = e.type === 'builds_on'
    const opacity = dimmed.has(e.from) || dimmed.has(e.to) ? scale.dimmed : 1
    // builds_on points from child to parent and is drawn parent to child, so the tree reads top
    // down. A cross link has no direction to show and is drawn from its upper end.
    const [source, target] = positions.get(e.from)!.y > positions.get(e.to)!.y ? [e.to, e.from] : [e.from, e.to]
    const a = positions.get(source)!
    const b = positions.get(target)!
    // Cross links keep to the gaps between rows, each kind of line at its own height: under the
    // row for two nodes of one row, low in the gap for neighbouring rows. Only a link that skips
    // a row has to cross it, from side to side.
    const rows = (b.y - a.y) / ROW
    // A node hangs one row under its deepest parent, so a builds_on that skips a row is a further
    // parent: it curves into the child's side, labelled, clear of the tree's own lines.
    const further = hierarchy && rows > 1
    // A cross link to the node's own parent would lie on the tree's line, so it loops round the side.
    const [sourceHandle, targetHandle] = further ? ['bottom', a.x < b.x ? 'left' : 'right']
      : !hierarchy && builtOn.has(`${e.from}>${e.to}`) ? ['left', 'left']
      : hierarchy || rows === 1 ? ['bottom', 'top']
      : rows === 0 ? ['bottom', 'bottom']
      : a.x < b.x ? ['right', 'left'] : ['left', 'right']
    return {
      id: `${e.type}:${e.from}:${e.to}`,
      source,
      target,
      sourceHandle,
      targetHandle,
      type: further || rows > 1 ? 'default' : 'smoothstep',
      pathOptions: hierarchy ? undefined : { stepPosition: 1, offset: e.type === 'related' ? 12 : 20 },
      label: far || (hierarchy && !further) ? undefined : LINK_KIND_LABELS[e.type],
      zIndex: hierarchy && !further ? 0 : 1,
      labelStyle: { ...chart.label, fill: s.text, opacity },
      labelBgStyle: { fill: semantic.surface },
      labelBgPadding: [4, 2] as [number, number],
      style: {
        stroke: s.color,
        strokeWidth: LINK_WIDTH[e.type],
        opacity,
      },
    }
  })}, [graph, positions, dimmed, far])

  // Opening in or leaving the whole window resizes the canvas: the view is placed again once it has.
  useEffect(() => {
    centered.current = ''
  }, [expanded])

  // Without a focus the view opens on the whole graph under the objective, or on its top when
  // the whole would be too small to read.
  useEffect(() => {
    if (!ready || width === 0) return
    const target = positions.has(focus) ? focus : graph.root
    const p = positions.get(target)!
    if (centered.current === target) return
    centered.current = target
    if (target === focus) {
      setCenter(p.x + NODE_WIDTH / 2, p.y + NODE_HEIGHT / 2, { zoom: Math.max(getZoom(), 0.8), duration: 300 })
    } else {
      const all = Array.from(positions.values())
      const halfWidth = Math.max(...all.map(q => Math.abs(q.x - p.x))) + NODE_WIDTH / 2
      const depth = Math.max(...all.map(q => q.y)) + NODE_HEIGHT
      const zoom = Math.min(0.8, Math.max(0.65, Math.min((width / 2 - 20) / halfWidth, (height - 60) / depth)))
      setViewport({ x: width / 2 - (p.x + NODE_WIDTH / 2) * zoom, y: 40 - p.y * zoom, zoom })
    }
  }, [ready, focus, graph.root, positions, width, height, setCenter, setViewport, getZoom])

  return (
    <Highlight.Provider value={highlight}>
      <Legend onTidy={onTidy} />
      <ReactFlow
        ref={flow}
        nodes={nodes}
        edges={edges}
        nodeTypes={nodeTypes}
        connectionMode={ConnectionMode.Loose}
        onInit={instance => {
          setZoom(instance.getZoom())
          setReady(true)
        }}
        onMoveEnd={(_, viewport) => setZoom(viewport.zoom)}
        minZoom={0.05}
        maxZoom={2}
        onlyRenderVisibleElements
        nodesDraggable={false}
        nodesConnectable={false}
        elementsSelectable={false}
        onNodeClick={(_, node) => (picking ? editor?.pick(node.id) : onSelect(node.id))}
        proOptions={{ hideAttribution: true }}
        style={{ background: semantic.surface, flex: 1, height: 'auto', minHeight: 0 }}
      >
        <Background gap={24} color={tint(semantic.text, 'track')} />
        <Controls showInteractive={false} position="top-left" />
        {graph.nodes.length === 0 && (
          <Panel position="bottom-center" className="text-xs text-muted">
            No hypotheses yet.
          </Panel>
        )}
        {minimap && graph.nodes.length >= MINIMAP_FROM && (
          <MiniMap
            position="top-right"
            pannable
            zoomable
            nodeColor={n => n.type === 'objective' ? semantic.primary : statusColor((n.data as HypothesisNodeData).item.status)}
            style={{ background: semantic.surfaceInset, width: 150, height: 100 }}
          />
        )}
      </ReactFlow>
    </Highlight.Provider>
  )
}
