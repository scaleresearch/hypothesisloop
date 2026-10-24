'use client'

import { actorLabel } from '@/components/actor'
import { useAgentName } from '@/components/agent-label'
import { Badge } from '@/components/ui/badge'
import { Claim } from '@/components/ui/claim'
import { TextLink } from '@/components/ui/link'
import { Section } from '@/components/ui/section'
import { LinkTr, Table, num } from '@/components/ui/table'
import { Text } from '@/components/ui/text'
import { formatMetric, headline, relTime, shortId } from '@/lib/format'
import { filedAfterVerdict } from '@/lib/hypothesis-progress'
import { experimentPath, hypothesisPath, jobPath } from '@/lib/routes'
import type { HypothesisEvidence, MetricDefinition } from '@/types/api'

const SHOWN = 6

/** The hypotheses with a verdict or a result, best result first, the best few with a link to all. */
export function Findings({ experimentId, findings, direction }: {
  experimentId: string
  findings: HypothesisEvidence[]
  direction: MetricDefinition['direction'] | undefined
}) {
  // A hypothesis whose jobs reported no metric comes last.
  const bestFirst = direction === 'minimize' ? 1 : -1
  const found = [...findings].sort((a, b) =>
    (a.best_job ? bestFirst * a.best_job.metric.value : Infinity) - (b.best_job ? bestFirst * b.best_job.metric.value : Infinity))
  const agentName = useAgentName()
  if (found.length === 0) return null
  const verdictBy = (h: HypothesisEvidence) => h.status_changed_at == null ? ''
    : `${h.status_actor ? `by ${actorLabel(h.status_actor, h.agent_id, agentName(h.agent_id))} · ` : ''}${relTime(h.status_changed_at)}`

  return (
    <Section
      title="Findings"
      note={`${found.length} hypotheses with a verdict or a result`}
      actions={found.length > SHOWN && <TextLink href={experimentPath(experimentId, 'hypotheses')} className="text-sm">All {found.length} hypotheses →</TextLink>}
      scrollX
    >
      <Table className="min-w-3xl table-fixed">
        <thead>
          <tr>
            <th className="w-28">Verdict</th>
            <th>Hypothesis</th>
            <th className="w-44 text-right">Best job</th>
            <th title="The best job's own summary. Without one, the newest finding of the hypothesis, named with the job it is about.">Finding</th>
          </tr>
        </thead>
        <tbody>
          {found.slice(0, SHOWN).map(h => (
            <LinkTr key={h.id} href={hypothesisPath(experimentId, h.id)} className="*:align-top">
              <td>
                <Badge status={h.status}>{h.status}</Badge>
                <Text as="div" variant="meta">{verdictBy(h)}</Text>
                {h.finding && filedAfterVerdict(h, h.finding.created_at) && (
                  <Text as="div" variant="meta" tone="warning" title="The newest finding was filed after this verdict was set">
                    newer finding
                  </Text>
                )}
              </td>
              <td className="text-xs">
                <TextLink onClick={e => e.stopPropagation()} href={hypothesisPath(experimentId, h.id)} className="block"><Claim text={h.text} lines={2} /></TextLink>
              </td>
              <td className={num}>
                {h.best_job ? (
                  <TextLink onClick={e => e.stopPropagation()} href={jobPath(experimentId, h.best_job.id)} title={h.best_job.id}>
                    {h.best_job.metric.name} {formatMetric(h.best_job.metric.value)}
                  </TextLink>
                ) : <span className="text-xs text-muted">—</span>}
              </td>
              <td className="text-xs">
                {h.best_job?.summary ? (
                  <span className="line-clamp-2" title={h.best_job.summary}>{headline(h.best_job.summary)}</span>
                ) : h.finding ? (
                  <span className="line-clamp-2" title={h.finding.summary}>
                    <TextLink onClick={e => e.stopPropagation()} href={jobPath(experimentId, h.finding.experiment_id)} title={h.finding.experiment_id}>{shortId(h.finding.experiment_id)}</TextLink>
                    {': '}{headline(h.finding.summary)}
                  </span>
                ) : <span className="text-xs text-muted">no finding filed</span>}
              </td>
            </LinkTr>
          ))}
        </tbody>
      </Table>
    </Section>
  )
}
