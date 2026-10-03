'use client'

import { TextLink } from '@/components/ui/link'
import type { HypothesisGraph, HypothesisGraphNode, HypothesisLinkType, PlatformExperiment } from '@/types'
import { Card, CardHeader, CardContent, CardTitle } from '@/components/ui/card'
import { Badge, Dot } from '@/components/ui/badge'
import { Button, RowButton } from '@/components/ui/button'
import { Eyebrow } from '@/components/ui/text'
import { EmptyState } from '@/components/ui/status-message'
import { CollapsibleDescription } from '@/components/ui/collapsible-description'
import { Actor, Verdict } from '@/components/actor'
import { linkGroups } from '@/lib/hypothesis-graph'
import { semantic, statusColor } from '@/lib/colors'
import { filedAfterVerdict } from '@/lib/hypothesis-progress'
import { relTime } from '@/lib/format'
import { useReadOnly } from '@/lib/read-only'

interface HypothesisPanelProps {
  graph: HypothesisGraph
  nodeByID: Map<string, HypothesisGraphNode>
  selected: string
  onSelect: (id: string) => void
  onAddIdea: (parentID: string) => void
}

export function ObjectiveBar({ graph, pe, onAddIdea }: {
  graph: HypothesisGraph
  pe: PlatformExperiment
  onAddIdea: (parentID: string) => void
}) {
  const readOnly = useReadOnly()
  const { finding, best_job: bestJob } = graph.objective
  return (
    <Card>
      <CardContent className="grid grid-cols-[minmax(0,1fr)] gap-2">
        <div className="flex flex-wrap items-baseline gap-3">
          <Eyebrow>Objective</Eyebrow>
          <span className="font-medium">{pe.name}</span>
          <span className="text-xs text-muted">
            {graph.nodes.length} hypotheses, {graph.nodes.filter(n => n.builds_on.includes(graph.root)).length} building directly on the objective
          </span>
          <span className="ml-auto flex items-center gap-3">
            {!readOnly && <Button size="sm" variant="primary" onClick={() => onAddIdea(graph.root)}>+ Add idea under the objective</Button>}
            <TextLink href={`/platform-experiments/${graph.root}`} className="text-xs">Experiment →</TextLink>
          </span>
        </div>
        {pe.description && <CollapsibleDescription text={pe.description} collapseLength={140} />}
        {bestJob && (
          <div className="font-mono text-xs">
            <span className="text-muted">Baseline job </span>
            <TextLink href={`/jobs/${bestJob.id}`}>{bestJob.metric.name} {bestJob.metric.value.toFixed(4)}</TextLink>
          </div>
        )}
        {finding && (
          <div className="grid grid-cols-[minmax(0,1fr)] gap-1">
            <Eyebrow>
              Baseline finding · <TextLink href={`/jobs/${finding.experiment_id}`} className="font-mono normal-case">{finding.experiment_id}</TextLink>
            </Eyebrow>
            <CollapsibleDescription text={finding.summary} collapseLength={140} />
          </div>
        )}
      </CardContent>
    </Card>
  )
}

export function HypothesisPanel({ graph, nodeByID, selected, onSelect, onAddIdea }: HypothesisPanelProps) {
  const readOnly = useReadOnly()
  const node = nodeByID.get(selected)
  if (!node) {
    return (
      <Card>
        <CardHeader><CardTitle>Hypothesis</CardTitle></CardHeader>
        <CardContent>
          <EmptyState>
            <span className="font-mono text-xs">{selected}</span> is not in this experiment&apos;s graph.{' '}
            <Button variant="link" onClick={() => onSelect('')}>Show the objective</Button>
          </EmptyState>
        </CardContent>
      </Card>
    )
  }

  const { finding, best_job: bestJob } = node
  const out = graph.edges.filter(e => e.from === node.id).map(e => ({ type: e.type, id: e.to }))
  const incoming = graph.edges.filter(e => e.to === node.id).map(e => ({ type: e.type, id: e.from }))

  return (
    <Card>
      <CardHeader>
        <CardTitle>Hypothesis</CardTitle>
        <Badge status={node.status}>{node.status}</Badge>
      </CardHeader>
      <CardContent className="grid grid-cols-[minmax(0,1fr)] gap-3">
        <p className="max-h-60 overflow-y-auto whitespace-pre-wrap break-words text-sm leading-relaxed">
          {node.text}
        </p>
        <div className="text-2xs text-muted">
          <Actor actor={node.actor} agentID={node.agent_id} /> · {relTime(node.created_at)}
          {node.status_changed_at && <> · <Verdict hypothesis={node} /></>}
        </div>
        <div className="font-mono text-2xs text-muted">
          {node.finding_count} findings · {node.comment_count} comments · built on by {node.child_count}
        </div>
        {bestJob && (
          <div className="font-mono text-xs">
            <span className="text-muted">Best job </span>
            <TextLink href={`/jobs/${bestJob.id}`}>{bestJob.metric.name} {bestJob.metric.value.toFixed(4)}</TextLink>
          </div>
        )}
        {finding && (
          <div className="grid grid-cols-[minmax(0,1fr)] gap-1">
            <Eyebrow>
              Newest finding · <TextLink href={`/jobs/${finding.experiment_id}`} className="font-mono normal-case">{finding.experiment_id}</TextLink>
              {filedAfterVerdict(node, finding.created_at) && <span className="text-warning"> · filed after the verdict</span>}
            </Eyebrow>
            <p className="max-h-40 overflow-y-auto whitespace-pre-wrap break-words text-xs">
              {finding.summary}
            </p>
          </div>
        )}
        {linkGroups(out, incoming).map(group => (
          <div key={group.label} className="grid grid-cols-[minmax(0,1fr)] gap-1">
            <Eyebrow>{group.label}</Eyebrow>
            {group.items.map(l => (
              <PanelLink key={`${l.type}:${l.id}`} id={l.id} type={l.type} graph={graph} nodeByID={nodeByID} onSelect={onSelect} />
            ))}
          </div>
        ))}
        <div className="flex flex-wrap items-center gap-3">
          {!readOnly && <Button size="sm" variant="primary" onClick={() => onAddIdea(node.id)}>+ Add idea under this</Button>}
          <TextLink href={`/hypotheses/${node.id}`} className="text-xs">Open detail →</TextLink>
        </div>
      </CardContent>
    </Card>
  )
}

function PanelLink({ id, type, graph, nodeByID, onSelect }: {
  id: string
  type: HypothesisLinkType
  graph: HypothesisGraph
  nodeByID: Map<string, HypothesisGraphNode>
  onSelect: (id: string) => void
}) {
  const target = nodeByID.get(id)
  const label = id === graph.root ? 'Objective' : target?.text ?? id
  return (
    <RowButton className="text-xs" onClick={() => onSelect(id)} title={label} aria-label={`${type}: ${label}`}>
      <Dot color={target ? statusColor(target.status) : semantic.accent} />
      <span className="min-w-0 flex-1 truncate">{label}</span>
    </RowButton>
  )
}
