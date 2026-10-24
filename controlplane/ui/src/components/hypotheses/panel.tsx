'use client'

import { useState } from 'react'

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
import { Disclosure } from '@/components/ui/disclosure'
import { Prose } from '@/components/ui/prose'
import { Curation } from '@/components/hypotheses/curation'
import { IdeaText } from '@/components/hypotheses/idea-text'
import { Verdict, Actor } from '@/components/actor'
import { linkGroups, objectiveFinding } from '@/lib/hypothesis-graph'
import { semantic, statusColor } from '@/lib/colors'
import { filedAfterVerdict } from '@/lib/hypothesis-progress'
import { formatMetric, headline, relTime, shortId } from '@/lib/format'
import { experimentPath, hypothesisPath, jobPath } from '@/lib/routes'
import type { GraphNode, HypothesisGraph, LinkType, PlatformExperiment } from '@/types/api'

interface HypothesisPanelProps {
  graph: HypothesisGraph
  nodeByID: Map<string, GraphNode>
  selected: string
  onSelect: (id: string) => void
}

const NEWEST = 3

export function ObjectiveBar({ graph, pe }: { graph: HypothesisGraph; pe: PlatformExperiment }) {
  const { best_job: bestJob } = graph.objective
  const shown = objectiveFinding(graph.objective)
  const comments = [...(graph.objective.comments ?? [])].reverse()
  const commentCount = graph.objective.comment_count ?? comments.length
  const [all, setAll] = useState(false)
  const linkJob = (ref: string) => (ref.startsWith('job-') ? jobPath(graph.root, ref) : undefined)
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
        {pe.description && <Prose text={pe.description.replace(/^\s*objective\s*:?\s*\n/i, '')} collapsedLines={2} />}
        {(bestJob || shown) && (
          <Stack gap="2xs">
            <Text as="div" variant="note" className="tabular-nums">
              Baseline{bestJob && <> {bestJob.metric.name} {formatMetric(bestJob.metric.value)}</>}
              {shown && <> · <TextLink href={jobPath(graph.root, shown.job)} title={shown.job} className="font-mono">{shortId(shown.job)}</TextLink></>}
            </Text>
            {shown && <Text as="div" variant="meta" tone="subtle" className="line-clamp-1" title={shown.summary}>{headline(shown.summary, 200)}</Text>}
          </Stack>
        )}
        {comments.length > 0 && (
          <Disclosure label={`Comments (${commentCount})`}>
            <Stack gap="sm">
              {(all ? comments : comments.slice(0, NEWEST)).map(c => (
                <Stack gap="2xs" key={c.id}>
                  <Text as="div" variant="meta"><Actor actor={c.actor} agentID={c.agent_id} /> · {relTime(c.created_at)}</Text>
                  <Prose text={c.text} collapsedLines={4} linkId={linkJob} />
                </Stack>
              ))}
              {comments.length > NEWEST && (
                <div>
                  <Button variant="link" size="sm" onClick={() => setAll(!all)}>
                    {all ? 'Only the newest' : `All ${comments.length}`}
                  </Button>
                  {all && commentCount > comments.length && <Text as="span" variant="hint"> The newest {comments.length} of {commentCount} are loaded.</Text>}
                </div>
              )}
            </Stack>
          </Disclosure>
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
        <Curation hypothesis={node} into={id => <Button variant="link" className="font-mono" title={id} onClick={() => onSelect(id)}>{shortId(id)}</Button>} />
        <Text as="div" variant="meta" mono>
          {node.finding_count} findings · {node.comment_count} comments · refined by {node.child_count}
        </Text>
        {bestJob && (
          <Text as="div" variant="code">
            <span className="text-muted">Best job </span>
            <TextLink href={jobPath(graph.root, bestJob.id)}>{bestJob.metric.name} {formatMetric(bestJob.metric.value)}</TextLink>
          </Text>
        )}
        {finding && (
          <Stack gap="xs">
            <Eyebrow>
              Newest finding · <TextLink href={jobPath(graph.root, finding.experiment_id)} title={finding.experiment_id} className="font-mono normal-case">{shortId(finding.experiment_id)}</TextLink>
              {filedAfterVerdict(node, finding.created_at) && <span className="text-warning"> · filed after the verdict</span>}
            </Eyebrow>
            <Prose text={finding.summary} collapsedLines={6} linkId={ref => (ref.startsWith('job-') ? jobPath(graph.root, ref) : undefined)} />
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
