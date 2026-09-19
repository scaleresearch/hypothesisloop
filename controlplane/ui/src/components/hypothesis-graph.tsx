'use client'

import { memo, useEffect, useMemo, useRef, useState } from 'react'
import {
  ReactFlow, ReactFlowProvider, Background, Controls, MiniMap, Handle, Panel,
  Position as Side, useReactFlow, useStore,
} from '@xyflow/react'
import type { Edge, Node, NodeProps, NodeTypes } from '@xyflow/react'
import '@xyflow/react/dist/style.css'
import { clsx } from 'clsx'
import { FileText, MessageSquare } from 'lucide-react'
import type { HypothesisGraph, HypothesisListItem } from '@/types'
import { hypothesisLinkStyles, hypothesisStatusColors } from '@/lib/colors'
import { NODE_HEIGHT, NODE_WIDTH, matchesFilters } from '@/lib/hypothesis-graph'
import type { GraphFilters, Position } from '@/lib/hypothesis-graph'

/** Owner and evidence counts, shared by graph nodes and outline rows. */
export function NodeMeta({ item }: { item: HypothesisListItem }) {
  return (
    <span className="hg-meta">
      {item.source === 'human'
        ? <span className="hg-meta-human hg-meta-owner" title={`human: ${item.author}`}>{item.author}</span>
        : <span className="hg-meta-owner" title={item.agent_id}>{item.agent_id}</span>}
      <span className="hg-count" title={`${item.finding_count} findings`}><FileText size={11} /> {item.finding_count}</span>
      <span className="hg-count" title={`${item.comment_count} comments`}><MessageSquare size={11} /> {item.comment_count}</span>
    </span>
  )
}

type HypothesisNodeData = { item: HypothesisListItem; selected: boolean; hit: boolean; dimmed: boolean }
type ObjectiveNodeData = { label: string; selected: boolean }

function HiddenHandles() {
  return (
    <>
      <Handle type="target" position={Side.Top} isConnectable={false} className="hg-handle" />
      <Handle type="source" position={Side.Bottom} isConnectable={false} className="hg-handle" />
    </>
  )
}

const HypothesisNode = memo(function HypothesisNode({ data }: NodeProps<Node<HypothesisNodeData>>) {
  const { item } = data
  const color = hypothesisStatusColors[item.status]
  return (
    <div
      className={clsx('hg-node', data.selected && 'selected', data.hit && 'hit', data.dimmed && 'dimmed')}
      style={{ borderLeftColor: color }}
      title={item.text}
    >
      <HiddenHandles />
      <div className="hg-node-text">{item.text}</div>
      <div className="hg-node-foot">
        <span className="hg-node-status" style={{ color }}>{item.status}</span>
        <NodeMeta item={item} />
      </div>
    </div>
  )
})

const ObjectiveNode = memo(function ObjectiveNode({ data }: NodeProps<Node<ObjectiveNodeData>>) {
  return (
    <div className={clsx('hg-node hg-objective', data.selected && 'selected')}>
      <HiddenHandles />
      <span className="uppercase-label">Objective</span>
      <div className="hg-node-text">{data.label}</div>
    </div>
  )
})

const nodeTypes: NodeTypes = { hypothesis: HypothesisNode, objective: ObjectiveNode }

function Legend() {
  return (
    <div className="hg-legend">
      {(Object.keys(hypothesisLinkStyles) as Array<keyof typeof hypothesisLinkStyles>).map(type => {
        const s = hypothesisLinkStyles[type]
        return (
          <span key={type}>
            <svg width="26" height="8" aria-hidden="true">
              <line x1="0" y1="4" x2="26" y2="4" stroke={s.color} strokeWidth="2" strokeDasharray={s.dash} />
            </svg>
            {s.label}
          </span>
        )
      })}
      {Object.entries(hypothesisStatusColors).map(([status, color]) => (
        <span key={status}>
          <span className="hg-legend-dot" style={{ background: color }} />
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
  /** The id named in the URL, centred on whenever it changes from outside the canvas. */
  focus: string
  selected: string
  hits: Set<string>
  filters: GraphFilters
  onSelect: (id: string) => void
}

export function HypothesisGraphCanvas(props: HypothesisGraphCanvasProps) {
  return (
    <ReactFlowProvider>
      <Canvas {...props} />
    </ReactFlowProvider>
  )
}

function Canvas({ graph, positions, rootLabel, focus, selected, hits, filters, onSelect }: HypothesisGraphCanvasProps) {
  const { setCenter, setViewport, getZoom } = useReactFlow()
  const width = useStore(s => s.width)
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
      data: { label: rootLabel, selected: selected === graph.root },
    },
    ...graph.nodes.filter(item => positions.has(item.id)).map(item => ({
      id: item.id,
      type: 'hypothesis',
      position: positions.get(item.id)!,
      width: NODE_WIDTH,
      height: NODE_HEIGHT,
      data: { item, selected: selected === item.id, hit: hits.has(item.id), dimmed: dimmed.has(item.id) },
    })),
  ], [graph, positions, rootLabel, selected, hits, dimmed])

  const edges = useMemo<Edge[]>(() => graph.edges.filter(e => positions.has(e.from) && positions.has(e.to)).map(e => {
    const s = hypothesisLinkStyles[e.type]
    const hierarchy = e.type === 'builds_on'
    return {
      id: `${e.type}:${e.from}:${e.to}`,
      // builds_on points from child to parent; drawn parent to child so the tree reads top down.
      source: hierarchy ? e.to : e.from,
      target: hierarchy ? e.from : e.to,
      type: hierarchy ? 'smoothstep' : 'default',
      style: {
        stroke: s.color,
        strokeWidth: hierarchy ? 1.5 : 2,
        strokeDasharray: s.dash,
        opacity: dimmed.has(e.from) || dimmed.has(e.to) ? 0.15 : 1,
      },
    }
  }), [graph, positions, dimmed])

  // Without a focus the view opens on the objective at the top centre: a few hundred nodes
  // framed at once are unreadable.
  useEffect(() => {
    if (!ready || width === 0) return
    const target = focus || graph.root
    const p = positions.get(target)
    if (!p || centered.current === target) return
    centered.current = target
    if (focus) {
      setCenter(p.x + NODE_WIDTH / 2, p.y + NODE_HEIGHT / 2, { zoom: Math.max(getZoom(), 0.8), duration: 300 })
    } else {
      setViewport({ x: width / 2 - (p.x + NODE_WIDTH / 2) * 0.8, y: 80 - p.y * 0.8, zoom: 0.8 })
    }
  }, [ready, focus, graph.root, positions, width, setCenter, setViewport, getZoom])

  return (
    <ReactFlow
      nodes={nodes}
      edges={edges}
      nodeTypes={nodeTypes}
      colorMode="dark"
      onInit={() => setReady(true)}
      minZoom={0.05}
      maxZoom={2}
      onlyRenderVisibleElements
      nodesDraggable={false}
      nodesConnectable={false}
      elementsSelectable={false}
      onNodeClick={(_, node) => {
        centered.current = node.id
        onSelect(node.id)
      }}
      attributionPosition="top-right"
      style={{ background: 'var(--surface)' }}
    >
      <Background gap={24} color="rgba(255, 255, 255, 0.05)" />
      <Controls showInteractive={false} />
      <MiniMap
        pannable
        zoomable
        nodeColor={n => n.type === 'objective' ? '#7c6cf0' : hypothesisStatusColors[(n.data as HypothesisNodeData).item.status]}
        maskColor="rgba(8, 9, 12, 0.7)"
        style={{ background: 'var(--surface-2)' }}
      />
      {/* The view opens on the objective at the top and centres a focused node, so the legend sits
          at the bottom, between the controls and the minimap, where it hides neither. */}
      <Panel position="bottom-center">
        <Legend />
      </Panel>
    </ReactFlow>
  )
}
