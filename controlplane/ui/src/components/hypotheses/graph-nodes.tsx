'use client'

import { createContext, memo, useContext } from 'react'
import { Handle, Position as Side, useStore } from '@xyflow/react'
import type { Node, NodeHandle, NodeProps, NodeTypes, ReactFlowState } from '@xyflow/react'
import { clsx } from 'clsx'
import { Plus } from 'lucide-react'
import type { HypothesisGraphNode, ObjectiveEvidence } from '@/types/hypotheses'
import { verdictBy } from '@/components/actor'
import { StatusLabel } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Eyebrow } from '@/components/ui/text'
import { semantic, statusColor, statusTone, tint } from '@/lib/colors'
import { NODE_HEIGHT, NODE_WIDTH } from '@/lib/hypothesis-graph'
import { addHereLabel } from '@/components/hypotheses/editor-controls'
import { NodeMeta, ObjectiveMeta } from '@/components/hypotheses/node-meta'

export type HypothesisNodeData = { item: HypothesisGraphNode }
type ObjectiveNodeData = { label: string; objective: ObjectiveEvidence }

// One data object per idea object, so a memoized node whose idea a poll left unchanged is skipped.
const nodeData = new WeakMap<HypothesisGraphNode, HypothesisNodeData>()
export function dataOf(item: HypothesisGraphNode): HypothesisNodeData {
  let data = nodeData.get(item)
  if (!data) nodeData.set(item, (data = { item }))
  return data
}

// Kept out of node data so that selecting or filtering re-renders only the drawn nodes instead of
// rebuilding every React Flow node.
export const Highlight = createContext({
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
export const NODE_HANDLES: NodeHandle[] = [
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
export const isFar = (s: ReactFlowState) => s.transform[2] < FAR_ZOOM
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

export const nodeTypes: NodeTypes = { hypothesis: HypothesisNode, objective: ObjectiveNode }
