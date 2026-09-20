'use client'

import Link from 'next/link'
import type { HypothesisGraph, HypothesisLinkType, HypothesisListItem, PlatformExperiment } from '@/types'
import type { HypothesisEvidence } from '@/lib/api'
import { Pod, PodHeader, PodContent } from '@/components/ui/pod'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { EmptyState } from '@/components/ui/status-message'
import { CollapsibleDescription } from '@/components/ui/collapsible-description'
import { PoolAuthor } from '@/components/human-idea'
import { linkGroups } from '@/lib/hypothesis-graph'
import { hypothesisStatusColors, semantic } from '@/lib/colors'
import { filedAfterVerdict, verdictSetBy } from '@/lib/hypothesis-progress'
import { relTime } from '@/lib/format'

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
  return (
    <Pod>
      <PodContent className="hg-stack">
        <div style={{ display: 'flex', alignItems: 'baseline', gap: 12, flexWrap: 'wrap' }}>
          <span className="uppercase-label">Objective</span>
          <span style={{ fontWeight: 650 }}>{pe.name}</span>
          <span className="text-muted" style={{ fontSize: 12 }}>
            {graph.nodes.length} hypotheses, {graph.nodes.filter(n => n.builds_on.includes(graph.root)).length} building directly on the objective
          </span>
          <span style={{ display: 'flex', alignItems: 'center', gap: 12, marginLeft: 'auto' }}>
            <Button size="sm" variant="primary" onClick={() => onAddIdea(graph.root)}>+ Add idea under the objective</Button>
            <Link href={`/platform-experiments/${graph.root}`} className="text-link" style={{ fontSize: 12 }}>Experiment →</Link>
          </span>
        </div>
        {pe.description && <CollapsibleDescription text={pe.description} collapseLength={140} />}
      </PodContent>
    </Pod>
  )
}

export function HypothesisPanel({ graph, nodeByID, evidence, selected, onSelect, onAddIdea }: HypothesisPanelProps) {
  const node = nodeByID.get(selected)
  if (!node) {
    return (
      <Pod>
        <PodHeader>Hypothesis</PodHeader>
        <PodContent>
          <EmptyState>
            <span className="mono">{selected}</span> is not in this experiment&apos;s graph.{' '}
            <Button variant="link" onClick={() => onSelect('')}>Show the objective</Button>
          </EmptyState>
        </PodContent>
      </Pod>
    )
  }

  const { finding, best_job: bestJob } = evidence.get(node.id) ?? {}
  const out = graph.edges.filter(e => e.from === node.id).map(e => ({ type: e.type, id: e.to }))
  const incoming = graph.edges.filter(e => e.to === node.id).map(e => ({ type: e.type, id: e.from }))

  return (
    <Pod>
      <PodHeader>
        Hypothesis
        <Badge status={node.status}>{node.status}</Badge>
      </PodHeader>
      <PodContent className="hg-stack" style={{ gap: 12 }}>
        <p style={{ fontSize: 13, lineHeight: 1.6, whiteSpace: 'pre-wrap', overflowWrap: 'break-word', maxHeight: 240, overflowY: 'auto' }}>
          {node.text}
        </p>
        <div className="text-muted" style={{ fontSize: 11 }}>
          <PoolAuthor source={node.source} agentID={node.agent_id} author={node.author} /> · {relTime(node.created_at)}
          {node.status_changed_at && <> · {node.status} {verdictSetBy(node)}</>}
        </div>
        <div className="text-muted mono" style={{ fontSize: 11 }}>
          {node.finding_count} findings · {node.comment_count} comments · built on by {node.child_count}
        </div>
        {bestJob && (
          <div className="mono" style={{ fontSize: 12 }}>
            <span className="text-muted">Best job </span>
            <Link href={`/jobs/${bestJob.id}`} className="text-link">{bestJob.metric.name} {bestJob.metric.best.toFixed(4)}</Link>
          </div>
        )}
        {finding && (
          <div className="hg-stack" style={{ gap: 4 }}>
            <span className="uppercase-label">
              Newest finding · <Link href={`/jobs/${finding.experiment_id}`} className="text-link mono" style={{ textTransform: 'none' }}>{finding.experiment_id}</Link>
              {filedAfterVerdict(node, finding.created_at) && <span style={{ color: semantic.warning }}> · filed after the verdict</span>}
            </span>
            <p style={{ fontSize: 12, lineHeight: 1.5, whiteSpace: 'pre-wrap', overflowWrap: 'break-word', maxHeight: 160, overflowY: 'auto' }}>
              {finding.summary}
            </p>
          </div>
        )}
        {linkGroups(out, incoming).map(group => (
          <div key={group.label} className="hg-stack" style={{ gap: 4 }}>
            <span className="uppercase-label">{group.label}</span>
            {group.items.map(l => (
              <PanelLink key={`${l.type}:${l.id}`} id={l.id} type={l.type} graph={graph} nodeByID={nodeByID} onSelect={onSelect} />
            ))}
          </div>
        ))}
        <div style={{ display: 'flex', alignItems: 'center', gap: 12, flexWrap: 'wrap' }}>
          <Button size="sm" variant="primary" onClick={() => onAddIdea(node.id)}>+ Add idea under this</Button>
          <Link href={`/hypotheses/${node.id}`} className="text-link" style={{ fontSize: 12 }}>Open detail →</Link>
        </div>
      </PodContent>
    </Pod>
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
    <button className="hg-hit" onClick={() => onSelect(id)} title={label} aria-label={`${type}: ${label}`}>
      <span className="hg-legend-dot" style={{ background: target ? hypothesisStatusColors[target.status] : 'var(--accent)' }} />
      <span className="hg-outline-text">{label}</span>
    </button>
  )
}
