'use client'

import { TextLink } from '@/components/ui/link'
import { Card, CardContent } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import { Button, RowButton } from '@/components/ui/button'
import { Dot } from '@/components/ui/dot'
import { EmptyState } from '@/components/ui/empty-state'
import { Section } from '@/components/ui/section'
import { Inline, Stack } from '@/components/ui/stack'
import { Eyebrow, Text } from '@/components/ui/text'
import { LinkButton } from '@/components/hypotheses/editor-controls'
import { IdeaText } from '@/components/hypotheses/idea-text'
import { CollapsibleDescription } from '@/components/ui/collapsible-description'
import { Actor, Verdict } from '@/components/actor'
import { linkGroups, objectiveFinding } from '@/lib/hypothesis-graph'
import { semantic, statusColor } from '@/lib/colors'
import { filedAfterVerdict } from '@/lib/hypothesis-progress'
import { relTime } from '@/lib/format'
import { experimentPath, hypothesisPath, jobPath } from '@/lib/routes'
import type { GraphNode, HypothesisGraph, LinkType, PlatformExperiment } from '@/types/api'

interface HypothesisPanelProps {
  graph: HypothesisGraph
  nodeByID: Map<string, GraphNode>
  selected: string
  onSelect: (id: string) => void
}

export function ObjectiveBar({ graph, pe }: { graph: HypothesisGraph; pe: PlatformExperiment }) {
  const { best_job: bestJob } = graph.objective
  const shown = objectiveFinding(graph.objective)
  const comments = graph.objective.comments ?? []
  const commentCount = graph.objective.comment_count ?? comments.length
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
          <TextLink href={experimentPath(graph.root)} className="ml-auto text-xs">Experiment →</TextLink>
        </Inline>
        {pe.description && <CollapsibleDescription text={pe.description} collapseLength={140} />}
        {bestJob && (
          <Text as="div" variant="note" className="tabular-nums">
            Baseline job{' '}
            <TextLink href={jobPath(graph.root, bestJob.id)}>{bestJob.metric.name} {bestJob.metric.value.toFixed(4)}</TextLink>
          </Text>
        )}
        {shown && (
          <Stack gap="xs">
            <Eyebrow>
              {shown.baseline ? 'Baseline finding' : 'Newest finding on the objective'} · <TextLink href={jobPath(graph.root, shown.job)} className="font-mono">{shown.job}</TextLink>
            </Eyebrow>
            <CollapsibleDescription text={shown.summary} collapseLength={140} />
          </Stack>
        )}
        {comments.length > 0 && (
          <Stack gap="xs">
            <Eyebrow>
              {commentCount} comments on the objective{commentCount > comments.length && `, the newest ${comments.length}`}
            </Eyebrow>
            {comments.map(c => (
              <Stack gap="xs" key={c.id}>
                <Text as="div" variant="meta"><Actor actor={c.actor} agentID={c.agent_id} /> · {relTime(c.created_at)}</Text>
                <CollapsibleDescription text={c.text} collapseLength={140} />
              </Stack>
            ))}
          </Stack>
        )}
        </Stack>
      </CardContent>
    </Card>
  )
}

export function HypothesisPanel({ graph, nodeByID, selected, onSelect }: HypothesisPanelProps) {
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
        <IdeaText node={node} />
        <Text as="div" variant="meta">
          <Actor actor={node.actor} agentID={node.agent_id} /> · {relTime(node.created_at)}
          {node.status_changed_at && <> · <Verdict hypothesis={node} /></>}
        </Text>
        <Text as="div" variant="meta" mono>
          {node.finding_count} findings · {node.comment_count} comments · refined by {node.child_count}
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
          <LinkButton id={node.id} />
          <TextLink href={hypothesisPath(graph.root, node.id)} className="text-xs">Open detail →</TextLink>
        </Inline>
      </Stack>
    </Section>
  )
}

function PanelLink({ id, type, graph, nodeByID, onSelect }: {
  id: string
  type: LinkType
  graph: HypothesisGraph
  nodeByID: Map<string, GraphNode>
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
