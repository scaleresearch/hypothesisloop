'use client'

import { Link } from '@/components/ui/link'
import type { HypothesisGraph, HypothesisLinkType, HypothesisListItem, PlatformExperiment } from '@/types'
import type { HypothesisEvidence } from '@/lib/api'
import { Card, CardHeader, CardContent } from '@/components/ui/card'
import { Badge, Dot } from '@/components/ui/badge'
import { Button, RowButton } from '@/components/ui/button'
import { Eyebrow } from '@/components/ui/text'
import { EmptyState } from '@/components/ui/status-message'
import { CollapsibleDescription } from '@/components/ui/collapsible-description'
import { PoolAuthor } from '@/components/human-idea'
import { linkGroups } from '@/lib/hypothesis-graph'
import { semantic, statusColor } from '@/lib/colors'
import { filedAfterVerdict, verdictSetBy } from '@/lib/hypothesis-progress'
import { relTime } from '@/lib/format'
import { useReadOnly } from '@/lib/example'

interface HypothesisPanelProps {
  graph: HypothesisGraph
  nodeByID: Map<string, HypothesisListItem>
  evidence: Map<string, HypothesisEvidence>
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
  return (
    <Card>
      <CardContent className="grid grid-cols-[minmax(0,1fr)] gap-2">
        <div className="flex flex-wrap items-baseline gap-3">
          <Eyebrow>Objective</Eyebrow>
          <span className="font-semibold">{pe.name}</span>
          <span className="text-xs text-muted">
            {graph.nodes.length} hypotheses, {graph.nodes.filter(n => n.builds_on.includes(graph.root)).length} building directly on the objective
          </span>
          <span className="ml-auto flex items-center gap-3">
            {!readOnly && <Button size="sm" variant="primary" onClick={() => onAddIdea(graph.root)}>+ Add idea under the objective</Button>}
            <Link href={`/platform-experiments/${graph.root}`} className="text-xs text-accent hover:underline">Experiment →</Link>
          </span>
        </div>
        {pe.description && <CollapsibleDescription text={pe.description} collapseLength={140} />}
      </CardContent>
    </Card>
  )
}

export function HypothesisPanel({ graph, nodeByID, evidence, selected, onSelect, onAddIdea }: HypothesisPanelProps) {
  const readOnly = useReadOnly()
  const node = nodeByID.get(selected)
  if (!node) {
    return (
      <Card>
        <CardHeader>Hypothesis</CardHeader>
        <CardContent>
          <EmptyState>
            <span className="font-mono text-xs">{selected}</span> is not in this experiment&apos;s graph.{' '}
            <Button variant="link" onClick={() => onSelect('')}>Show the objective</Button>
          </EmptyState>
        </CardContent>
      </Card>
    )
  }

  const { finding, best_job: bestJob } = evidence.get(node.id) ?? {}
  const out = graph.edges.filter(e => e.from === node.id).map(e => ({ type: e.type, id: e.to }))
  const incoming = graph.edges.filter(e => e.to === node.id).map(e => ({ type: e.type, id: e.from }))

  return (
    <Card>
      <CardHeader>
        Hypothesis
        <Badge status={node.status}>{node.status}</Badge>
      </CardHeader>
      <CardContent className="grid grid-cols-[minmax(0,1fr)] gap-3">
        <p className="max-h-60 overflow-y-auto whitespace-pre-wrap break-words text-sm leading-relaxed">
          {node.text}
        </p>
        <div className="text-2xs text-muted">
          <PoolAuthor source={node.source} agentID={node.agent_id} author={node.author} /> · {relTime(node.created_at)}
          {node.status_changed_at && <> · {node.status} {verdictSetBy(node)}</>}
        </div>
        <div className="font-mono text-2xs text-muted">
          {node.finding_count} findings · {node.comment_count} comments · built on by {node.child_count}
        </div>
        {bestJob && (
          <div className="font-mono text-xs">
            <span className="text-muted">Best job </span>
            <Link href={`/jobs/${bestJob.id}`} className="text-accent hover:underline">{bestJob.metric.name} {bestJob.metric.best.toFixed(4)}</Link>
          </div>
        )}
        {finding && (
          <div className="grid grid-cols-[minmax(0,1fr)] gap-1">
            <Eyebrow>
              Newest finding · <Link href={`/jobs/${finding.experiment_id}`} className="font-mono normal-case text-accent hover:underline">{finding.experiment_id}</Link>
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
          <Link href={`/hypotheses/${node.id}`} className="text-xs text-accent hover:underline">Open detail →</Link>
        </div>
      </CardContent>
    </Card>
  )
}

function PanelLink({ id, type, graph, nodeByID, onSelect }: {
  id: string
  type: HypothesisLinkType
  graph: HypothesisGraph
  nodeByID: Map<string, HypothesisListItem>
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
