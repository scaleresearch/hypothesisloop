'use client'

import { createContext, memo, useContext, useEffect, useMemo, useRef, useState } from 'react'
import {
  ReactFlow, ReactFlowProvider, Background, ConnectionMode, Controls, MiniMap, Handle,
  Position as Side, useReactFlow, useStore,
} from '@xyflow/react'
import type { Edge, Node, NodeProps, NodeTypes } from '@xyflow/react'
import '@xyflow/react/dist/style.css'
import { clsx } from 'clsx'
import { FileText, MessageSquare } from 'lucide-react'
import type { Evidence, HypothesisGraph, HypothesisGraphNode } from '@/types'
import { hypothesisLinkStyles, hypothesisStatuses, semantic, statusColor, tint } from '@/lib/colors'
import { Dot } from '@/components/ui/badge'
import { Eyebrow } from '@/components/ui/text'
import { filedAfterVerdict } from '@/lib/hypothesis-progress'
import { Actor, verdictBy } from '@/components/actor'
import { NODE_HEIGHT, NODE_WIDTH, ROW, matchesFilters } from '@/lib/hypothesis-graph'
import type { GraphFilters, Position } from '@/lib/hypothesis-graph'

const COUNT = 'inline-flex items-center gap-0.5'

/** Who registered it, evidence counts and best result, shared by graph nodes and outline rows. */
export function NodeMeta({ item }: { item: HypothesisGraphNode }) {
  const newer = item.finding !== undefined && filedAfterVerdict(item, item.finding.created_at)
  return (
    <span className="inline-flex min-w-0 items-center gap-2 whitespace-nowrap font-mono text-2xs text-muted">
      <Actor actor={item.actor} agentID={item.agent_id} compact />
      <span
        className={clsx(COUNT, newer && 'text-warning')}
        title={`${item.finding_count} findings${newer ? ', the newest filed after the verdict' : ''}`}
      >
        <FileText size={11} /> {item.finding_count}
      </span>
      <span className={COUNT} title={`${item.comment_count} comments`}><MessageSquare size={11} /> {item.comment_count}</span>
      {item.best_job && (
        <span className={clsx(COUNT, 'text-foreground')} title={`Best ${item.best_job.metric.name} of its jobs`}>
          {item.best_job.metric.value.toFixed(4)}
        </span>
      )}
    </span>
  )
}

/** What the baseline found, on the objective its jobs hang on: shared by the graph's root node and the outline's root row. */
export function ObjectiveMeta({ objective }: { objective: Evidence }) {
  return (
    <span className="inline-flex items-center gap-2 whitespace-nowrap font-mono text-2xs text-muted">
      {objective.finding && (
        <span className={COUNT} title={`Baseline finding: ${objective.finding.summary}`}><FileText size={11} aria-label="Baseline finding" /></span>
      )}
      {objective.best_job && (
        <span className={clsx(COUNT, 'text-foreground')} title={`Best ${objective.best_job.metric.name} of the baseline's jobs`}>
          baseline {objective.best_job.metric.value.toFixed(4)}
        </span>
      )}
    </span>
  )
}

type HypothesisNodeData = { item: HypothesisGraphNode }
type ObjectiveNodeData = { label: string; objective: Evidence }

// Kept out of node data so that selecting or filtering re-renders only the drawn nodes instead of
// rebuilding every React Flow node.
const Highlight = createContext({ selected: '', hits: new Set<string>(), dimmed: new Set<string>() })

const HANDLE = 'pointer-events-none opacity-0'

// The box is NODE_WIDTH by NODE_HEIGHT: the layout places nodes by those.
const NODE = 'flex h-[92px] w-[240px] cursor-pointer flex-col gap-1.5 rounded border border-l-[3px] py-2 pl-3 pr-2.5 text-foreground transition-[opacity,box-shadow,background-color] hover:bg-surface-raised'
const NODE_TEXT = 'line-clamp-3 text-xs leading-snug [overflow-wrap:anywhere]'

function HiddenHandles() {
  return (
    <>
      <Handle id="top" type="target" position={Side.Top} isConnectable={false} className={HANDLE} />
      <Handle id="bottom" type="source" position={Side.Bottom} isConnectable={false} className={HANDLE} />
      <Handle id="left" type="source" position={Side.Left} isConnectable={false} className={HANDLE} />
      <Handle id="right" type="source" position={Side.Right} isConnectable={false} className={HANDLE} />
    </>
  )
}

const HypothesisNode = memo(function HypothesisNode({ data }: NodeProps<Node<HypothesisNodeData>>) {
  const { item } = data
  const { selected, hits, dimmed } = useContext(Highlight)
  const color = statusColor(item.status)
  return (
    <div
      className={clsx(
        NODE,
        'justify-between border-border-strong',
        selected === item.id ? 'bg-surface-raised shadow-md ring-1 ring-accent' : 'bg-surface-inset',
        hits.has(item.id) && 'ring-2 ring-warning',
        dimmed.has(item.id) && 'opacity-20',
      )}
      style={{ borderLeftColor: color }}
      title={item.text}
    >
      <HiddenHandles />
      <div className={NODE_TEXT}>{item.text}</div>
      <div className="flex min-w-0 items-center gap-2">
        <span className="font-mono text-2xs font-bold uppercase tracking-wide" style={{ color }} title={verdictBy(item) && `${item.status} ${verdictBy(item)}`}>{item.status}</span>
        <NodeMeta item={item} />
      </div>
    </div>
  )
})

const ObjectiveNode = memo(function ObjectiveNode({ id, data }: NodeProps<Node<ObjectiveNodeData>>) {
  const { selected } = useContext(Highlight)
  return (
    <div className={clsx(NODE, 'justify-center border-primary', selected === id ? 'bg-surface-raised shadow-md ring-1 ring-accent' : 'bg-primary/5')}>
      <HiddenHandles />
      <div className="flex items-center justify-between gap-2">
        <Eyebrow>Objective</Eyebrow>
        <ObjectiveMeta objective={data.objective} />
      </div>
      <div className={NODE_TEXT}>{data.label}</div>
    </div>
  )
})

const nodeTypes: NodeTypes = { hypothesis: HypothesisNode, objective: ObjectiveNode }

function Legend() {
  return (
    <div className="flex flex-wrap gap-x-3 gap-y-1 border-b px-3 py-1.5 text-2xs text-muted">
      {(Object.keys(hypothesisLinkStyles) as Array<keyof typeof hypothesisLinkStyles>).map(type => {
        const s = hypothesisLinkStyles[type]
        return (
          <span key={type} className="inline-flex items-center gap-1.5">
            <svg width="26" height="8" aria-hidden="true">
              <line x1="0" y1="4" x2="26" y2="4" stroke={s.color} strokeWidth="2" />
            </svg>
            {s.label}
          </span>
        )
      })}
      {hypothesisStatuses.map(status => (
        <span key={status} className="inline-flex items-center gap-1.5">
          <Dot color={statusColor(status)} />
          {status}
        </span>
      ))}
    </div>
  )
}

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
}

export function HypothesisGraphCanvas(props: HypothesisGraphCanvasProps) {
  return (
    <ReactFlowProvider>
      <Canvas {...props} />
    </ReactFlowProvider>
  )
}

function Canvas({ graph, positions, rootLabel, focus, selected, hits, filters, minimap, onSelect }: HypothesisGraphCanvasProps) {
  const { setCenter, setViewport, getZoom } = useReactFlow()
  const width = useStore(s => s.width)
  const height = useStore(s => s.height)
  const [ready, setReady] = useState(false)
  const centered = useRef('')

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
      data: { label: rootLabel, objective: graph.objective },
    },
    ...graph.nodes.filter(item => positions.has(item.id)).map(item => ({
      id: item.id,
      type: 'hypothesis',
      position: positions.get(item.id)!,
      width: NODE_WIDTH,
      height: NODE_HEIGHT,
      data: { item },
    })),
  ], [graph, positions, rootLabel])
  const highlight = useMemo(() => ({ selected, hits, dimmed }), [selected, hits, dimmed])

  const edges = useMemo<Edge[]>(() => {
    const builtOn = new Set(graph.edges.filter(e => e.type === 'builds_on').flatMap(e => [`${e.from}>${e.to}`, `${e.to}>${e.from}`]))
    return graph.edges.filter(e => positions.has(e.from) && positions.has(e.to)).map(e => {
    const s = hypothesisLinkStyles[e.type]
    const hierarchy = e.type === 'builds_on'
    const opacity = dimmed.has(e.from) || dimmed.has(e.to) ? 0.15 : 1
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
    // parent: it curves into the child's side, dashed and labelled, clear of the tree's own lines.
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
      label: hierarchy && !further ? undefined : s.label,
      zIndex: hierarchy && !further ? 0 : 1,
      labelStyle: { fill: s.color, fontSize: 10, fontWeight: 600, opacity },
      labelBgStyle: { fill: semantic.surface },
      labelBgPadding: [4, 2] as [number, number],
      style: {
        stroke: s.color,
        strokeWidth: hierarchy ? 1.5 : 2,
        strokeDasharray: further ? '6 4' : undefined,
        opacity,
      },
    }
  })}, [graph, positions, dimmed])

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
      const zoom = Math.min(0.8, Math.max(0.4, Math.min((width / 2 - 20) / halfWidth, (height - 60) / depth)))
      setViewport({ x: width / 2 - (p.x + NODE_WIDTH / 2) * zoom, y: 40 - p.y * zoom, zoom })
    }
  }, [ready, focus, graph.root, positions, width, height, setCenter, setViewport, getZoom])

  return (
    <Highlight.Provider value={highlight}>
      <Legend />
      <ReactFlow
        nodes={nodes}
        edges={edges}
        nodeTypes={nodeTypes}
        connectionMode={ConnectionMode.Loose}
        colorMode="system"
        onInit={() => setReady(true)}
        minZoom={0.05}
        maxZoom={2}
        onlyRenderVisibleElements
        nodesDraggable={false}
        nodesConnectable={false}
        elementsSelectable={false}
        onNodeClick={(_, node) => onSelect(node.id)}
        proOptions={{ hideAttribution: true }}
        style={{ background: semantic.surface, flex: 1, height: 'auto', minHeight: 0 }}
      >
        <Background gap={24} color={tint(semantic.text, 0.06)} />
        <Controls showInteractive={false} position="top-left" />
        {minimap && (
          <MiniMap
            position="top-right"
            pannable
            zoomable
            nodeColor={n => n.type === 'objective' ? semantic.primary : statusColor((n.data as HypothesisNodeData).item.status)}
            maskColor={tint(semantic.surface, 0.7)}
            style={{ background: semantic.surfaceInset, width: 150, height: 100 }}
          />
        )}
      </ReactFlow>
    </Highlight.Provider>
  )
}
