'use client'

import { createContext, memo, useContext, useEffect, useMemo, useRef, useState } from 'react'
import {
  ReactFlow, ReactFlowProvider, Background, ConnectionMode, Controls, MiniMap, Handle, Panel,
  Position as Side, useReactFlow, useStore,
} from '@xyflow/react'
import type { Edge, Node, NodeHandle, NodeProps, NodeTypes, ReactFlowState } from '@xyflow/react'
import '@xyflow/react/dist/style.css'
import { clsx } from 'clsx'
import { FileText, MessageSquare, Plus } from 'lucide-react'
import type { HypothesisGraph, HypothesisGraphNode, HypothesisLinkType, ObjectiveEvidence } from '@/types/hypotheses'
import { chart, hypothesisLinkStyles, hypothesisStatuses, semantic, statusColor, statusTone, tint } from '@/lib/colors'
import { opacity as scale } from '@/lib/scale'
import { Button } from '@/components/ui/button'
import { ExpandButton, useExpanded } from '@/components/ui/expandable'
import { Dot } from '@/components/ui/dot'
import { StatusLabel } from '@/components/ui/badge'
import { Inline } from '@/components/ui/stack'
import { Eyebrow, Text } from '@/components/ui/text'
import { filedAfterVerdict } from '@/lib/hypothesis-progress'
import { Actor, verdictBy } from '@/components/actor'
import { addHereLabel, useEditor } from '@/components/graph-edit'
import { LINK_KIND_LABELS, NODE_HEIGHT, NODE_WIDTH, ROW, matchesFilters, objectiveFinding } from '@/lib/hypothesis-graph'
import type { GraphFilters, Position } from '@/lib/hypothesis-graph'

// Kinds read apart by line as well as colour: a related link is thin, the others thick. No line is
// dashed: a dash pattern on a long path is rasterized dash by dash, which at hundreds of links
// stalls every frame of a pan or zoom.
const LINK_WIDTH: Record<HypothesisLinkType, number> = { builds_on: 1.5, related: 1, contradicts: 2.5 }

const COUNT = 'inline-flex items-center gap-0.5'

/** Who registered it, evidence counts and best result, shared by graph nodes and outline rows. */
export function NodeMeta({ item }: { item: HypothesisGraphNode }) {
  const newer = item.finding !== undefined && filedAfterVerdict(item, item.finding.created_at)
  return (
    <Text as="span" variant="meta" className="inline-flex min-w-0 items-center gap-2 whitespace-nowrap tabular-nums">
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
    </Text>
  )
}

/** What the baseline found, on the objective its jobs hang on: shared by the graph's root node and the outline's root row. */
export function ObjectiveMeta({ objective }: { objective: ObjectiveEvidence }) {
  const shown = objectiveFinding(objective)
  const comments = objective.comment_count ?? 0
  return (
    <Text as="span" variant="meta" className="inline-flex items-center gap-2 whitespace-nowrap tabular-nums">
      {comments > 0 && (
        <span className={COUNT} title={`${comments} comments on the objective`}><MessageSquare size={11} /> {comments}</span>
      )}
      {shown && (
        <span className={COUNT} title={`${shown.baseline ? 'Baseline finding' : 'Newest finding'}: ${shown.summary}`}><FileText size={11} aria-label={shown.baseline ? 'Baseline finding' : 'Newest finding'} /></span>
      )}
      {objective.best_job && (
        <span className={clsx(COUNT, 'text-foreground')} title={`Best ${objective.best_job.metric.name} of the baseline's jobs`}>
          baseline {objective.best_job.metric.value.toFixed(4)}
        </span>
      )}
    </Text>
  )
}

type HypothesisNodeData = { item: HypothesisGraphNode }
type ObjectiveNodeData = { label: string; objective: ObjectiveEvidence }

// One data object per idea object, so a memoized node whose idea a poll left unchanged is skipped.
const nodeData = new WeakMap<HypothesisGraphNode, HypothesisNodeData>()
function dataOf(item: HypothesisGraphNode): HypothesisNodeData {
  let data = nodeData.get(item)
  if (!data) nodeData.set(item, (data = { item }))
  return data
}

// A small graph is seen whole on the canvas; the map only takes room from it.
const MINIMAP_FROM = 15

// Kept out of node data so that selecting or filtering re-renders only the drawn nodes instead of
// rebuilding every React Flow node.
const Highlight = createContext({
  selected: '', hits: new Set<string>(), dimmed: new Set<string>(),
  /** While a link target is being picked: the nodes that can take it. */
  pickable: null as Set<string> | null,
  onAdd: null as ((id: string) => void) | null,
})

function pickClass(pickable: Set<string> | null, id: string) {
  if (!pickable) return undefined
  return pickable.has(id) ? 'cursor-crosshair ring-1 ring-primary' : 'cursor-not-allowed opacity-dimmed'
}

// Points at the middle of each side of the node's box. Given on every node, so React Flow places
// edges without first drawing and measuring every node, which lets it draw only the nodes in view.
// The drawn handles sit on the same points, so measuring a node once in view changes nothing.
const HANDLE = 'pointer-events-none !min-h-0 !min-w-0 !h-0 !w-0 !border-0 opacity-0'
const NODE_HANDLES: NodeHandle[] = [
  { id: 'top', type: 'target', position: Side.Top, x: NODE_WIDTH / 2, y: 0, width: 0, height: 0 },
  { id: 'bottom', type: 'source', position: Side.Bottom, x: NODE_WIDTH / 2, y: NODE_HEIGHT, width: 0, height: 0 },
  { id: 'left', type: 'source', position: Side.Left, x: 0, y: NODE_HEIGHT / 2, width: 0, height: 0 },
  { id: 'right', type: 'source', position: Side.Right, x: NODE_WIDTH, y: NODE_HEIGHT / 2, width: 0, height: 0 },
]

// Drawn at screen size whatever the zoom: the canvas keeps its zoom in --zoom, set once a zoom
// ends, since changing it restyles every drawn node.
function NodeAdd({ id, objective }: { id: string; objective: boolean }) {
  const { onAdd } = useContext(Highlight)
  if (!onAdd) return null
  const label = addHereLabel(objective)
  return (
    <Button
      variant="primary"
      size="icon"
      className="nodrag nopan absolute -bottom-2.5 -right-2.5 rounded-full opacity-0 focus-visible:opacity-100 group-hover:opacity-100"
      style={{ transform: 'scale(calc(1 / var(--zoom, 1)))' }}
      aria-label={label}
      title={label}
      onClick={e => {
        e.stopPropagation()
        onAdd(id)
      }}
    >
      <Plus size={12} />
    </Button>
  )
}

// Below this zoom a node's text is too small to read: nodes are drawn as blocks of their status
// colour and links without labels, so a view of the whole graph stays light to draw.
const FAR_ZOOM = 0.4
const isFar = (s: ReactFlowState) => s.transform[2] < FAR_ZOOM
const FAR_NODE = 'rounded border border-border-strong'

const NODE = 'group relative flex cursor-pointer flex-col gap-1.5 rounded border border-l-3 py-2 pl-3 pr-2.5 text-foreground transition hover:bg-surface-raised'
// The box is NODE_WIDTH by NODE_HEIGHT: the layout places nodes by those.
const NODE_BOX = { width: NODE_WIDTH, height: NODE_HEIGHT }
const NODE_TEXT = 'line-clamp-3 break-words text-sm'

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
  const { selected, hits, dimmed, pickable } = useContext(Highlight)
  const far = useStore(isFar)
  const color = statusColor(item.status)
  if (far) {
    return (
      <>
      <HiddenHandles />
      <div
        className={clsx(FAR_NODE, selected === item.id && 'ring-4 ring-accent', hits.has(item.id) && 'ring-4 ring-warning', dimmed.has(item.id) && 'opacity-dimmed', pickClass(pickable, item.id))}
        style={{ ...NODE_BOX, background: tint(color, 'edge') }}
        title={item.text}
      />
      </>
    )
  }
  return (
    <>
    <HiddenHandles />
    <div
      className={clsx(
        NODE,
        'justify-between border-border-strong',
        selected === item.id ? 'bg-surface-raised shadow-md ring-1 ring-accent' : 'bg-surface-inset',
        hits.has(item.id) && 'ring-2 ring-warning',
        dimmed.has(item.id) && 'opacity-dimmed',
        pickClass(pickable, item.id),
      )}
      style={{ ...NODE_BOX, borderLeftColor: color }}
      title={item.text}
    >
      <NodeAdd id={item.id} objective={false} />
      <div className={NODE_TEXT}>{item.text}</div>
      <div className="flex min-w-0 items-center gap-2">
        <StatusLabel tone={statusTone(item.status)} title={verdictBy(item) && `${item.status} ${verdictBy(item)}`} className="flex-none">
          <span className="inline-block lowercase first-letter:uppercase">{item.status}</span>
        </StatusLabel>
        <NodeMeta item={item} />
      </div>
    </div>
    </>
  )
})

const ObjectiveNode = memo(function ObjectiveNode({ id, data }: NodeProps<Node<ObjectiveNodeData>>) {
  const { selected, pickable } = useContext(Highlight)
  const far = useStore(isFar)
  if (far) {
    return (
      <>
      <HiddenHandles />
      <div className={clsx(FAR_NODE, 'border-primary', selected === id && 'ring-4 ring-accent', pickClass(pickable, id))} style={{ ...NODE_BOX, background: semantic.surfaceInset }} title={data.label} />
      </>
    )
  }
  return (
    <>
    <HiddenHandles />
    <div className={clsx(NODE, 'justify-center border-primary', selected === id ? 'bg-surface-raised shadow-md ring-1 ring-accent' : 'bg-surface-inset', pickClass(pickable, id))} style={NODE_BOX}>
      <div className="flex items-center justify-between gap-2">
        <Eyebrow>Objective</Eyebrow>
        <ObjectiveMeta objective={data.objective} />
      </div>
      <NodeAdd id={id} objective />
      <div className={NODE_TEXT}>{data.label}</div>
    </div>
    </>
  )
})

const nodeTypes: NodeTypes = { hypothesis: HypothesisNode, objective: ObjectiveNode }

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
      {hypothesisStatuses.map(status => (
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
