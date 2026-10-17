import { clsx } from 'clsx'
import { FileText, MessageSquare } from 'lucide-react'
import { Actor } from '@/components/actor'
import { Text } from '@/components/ui/text'
import { objectiveFinding } from '@/lib/hypothesis-graph'
import { filedAfterVerdict } from '@/lib/hypothesis-progress'
import { formatMetric } from '@/lib/format'
import type { GraphNode, ObjectiveEvidence } from '@/types/api'

const COUNT = 'inline-flex items-center gap-0.5'

/** Who registered it, evidence counts and best result, shared by graph nodes and outline rows. */
export function NodeMeta({ item }: { item: GraphNode }) {
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
          {formatMetric(item.best_job.metric.value)}
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
          baseline {formatMetric(objective.best_job.metric.value)}
        </span>
      )}
    </Text>
  )
}
