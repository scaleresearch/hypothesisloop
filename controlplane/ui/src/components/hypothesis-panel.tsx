'use client'

import { TextLink } from '@/components/ui/link'
import type { HypothesisGraph, HypothesisGraphNode, HypothesisLinkType, PlatformExperiment } from '@/types'
import { Card, CardContent } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import { Button, RowButton } from '@/components/ui/button'
import { Dot } from '@/components/ui/dot'
import { EmptyState } from '@/components/ui/empty-state'
import { Section } from '@/components/ui/section'
import { Inline, Stack } from '@/components/ui/stack'
import { Eyebrow, Text } from '@/components/ui/text'
import { ClosedNote, useEditor } from '@/components/graph-edit'
import { CollapsibleDescription } from '@/components/ui/collapsible-description'
import { Actor, Verdict } from '@/components/actor'
import { linkGroups } from '@/lib/hypothesis-graph'
import { semantic, statusColor } from '@/lib/colors'
import { filedAfterVerdict } from '@/lib/hypothesis-progress'
import { relTime } from '@/lib/format'
import { useReadOnly } from '@/lib/read-only'
import { experimentPath, hypothesisPath, jobPath } from '@/lib/routes'

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
  const closed = useEditor()?.closed ?? false
  const { finding, best_job: bestJob } = graph.objective
  return (
    <Card>
      <CardContent>
        <Stack gap="sm">
        <Inline gap="md" align="baseline">
          <Eyebrow>Objective</Eyebrow>
          <span className="font-medium">{pe.name}</span>
          <Text as="span" variant="note">
            {graph.nodes.length} hypotheses, {graph.nodes.filter(n => n.builds_on.includes(graph.root)).length} building directly on the objective
          </Text>
          <span className="ml-auto flex items-center gap-3">
            {!readOnly ? <Button size="sm" variant="primary" onClick={() => onAddIdea(graph.root)}>+ Add idea under the objective</Button> : closed && <ClosedNote />}
            <TextLink href={experimentPath(graph.root)} className="text-xs">Experiment →</TextLink>
          </span>
        </Inline>
        {pe.description && <CollapsibleDescription text={pe.description} collapseLength={140} />}
        {bestJob && (
          <Text as="div" variant="code">
            <span className="text-muted">Baseline job </span>
            <TextLink href={jobPath(graph.root, bestJob.id)}>{bestJob.metric.name} {bestJob.metric.value.toFixed(4)}</TextLink>
          </Text>
        )}
        {finding && (
          <Stack gap="xs">
            <Eyebrow>
              Baseline finding · <TextLink href={jobPath(graph.root, finding.experiment_id)} className="font-mono normal-case">{finding.experiment_id}</TextLink>
            </Eyebrow>
            <CollapsibleDescription text={finding.summary} collapseLength={140} />
          </Stack>
        )}
        </Stack>
      </CardContent>
    </Card>
  )
}

export function HypothesisPanel({ graph, nodeByID, selected, onSelect, onAddIdea }: HypothesisPanelProps) {
  const readOnly = useReadOnly()
  const closed = useEditor()?.closed ?? false
  const node = nodeByID.get(selected)
  if (!node) {
    return (
      <Section title="Hypothesis">
        <EmptyState>
          <Text as="span" variant="code">{selected}</Text> is not in this experiment&apos;s graph.{' '}
          <Button variant="link" onClick={() => onSelect('')}>Show the objective</Button>
        </EmptyState>
      </Section>
    )
  }

  const { finding, best_job: bestJob } = node
  const out = graph.edges.filter(e => e.from === node.id).map(e => ({ type: e.type, id: e.to }))
  const incoming = graph.edges.filter(e => e.to === node.id).map(e => ({ type: e.type, id: e.from }))

  return (
    <Section title="Hypothesis" actions={<Badge status={node.status}>{node.status}</Badge>}>
      <Stack>
        <Text pre className="max-h-60 overflow-y-auto leading-relaxed">
          {node.text}
        </Text>
        <Text as="div" variant="meta">
          <Actor actor={node.actor} agentID={node.agent_id} /> · {relTime(node.created_at)}
          {node.status_changed_at && <> · <Verdict hypothesis={node} /></>}
        </Text>
        <Text as="div" variant="meta" mono>
          {node.finding_count} findings · {node.comment_count} comments · built on by {node.child_count}
        </Text>
        {bestJob && (
          <Text as="div" variant="code">
            <span className="text-muted">Best job </span>
            <TextLink href={jobPath(graph.root, bestJob.id)}>{bestJob.metric.name} {bestJob.metric.value.toFixed(4)}</TextLink>
          </Text>
        )}
        {finding && (
          <Stack gap="xs">
            <Eyebrow>
              Newest finding · <TextLink href={jobPath(graph.root, finding.experiment_id)} className="font-mono normal-case">{finding.experiment_id}</TextLink>
              {filedAfterVerdict(node, finding.created_at) && <span className="text-warning"> · filed after the verdict</span>}
            </Eyebrow>
            <Text variant="note" tone="default" pre className="max-h-40 overflow-y-auto">
              {finding.summary}
            </Text>
          </Stack>
        )}
        {linkGroups(out, incoming).map(group => (
          <Stack gap="xs" key={group.label}>
            <Eyebrow>{group.label}</Eyebrow>
            {group.items.map(l => (
              <PanelLink key={`${l.type}:${l.id}`} id={l.id} type={l.type} graph={graph} nodeByID={nodeByID} onSelect={onSelect} />
            ))}
          </Stack>
        ))}
        <Inline gap="md">
          {!readOnly ? <Button size="sm" variant="primary" onClick={() => onAddIdea(node.id)}>+ Add idea under this</Button> : closed && <ClosedNote />}
          <TextLink href={hypothesisPath(graph.root, node.id)} className="text-xs">Open detail →</TextLink>
        </Inline>
      </Stack>
    </Section>
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
