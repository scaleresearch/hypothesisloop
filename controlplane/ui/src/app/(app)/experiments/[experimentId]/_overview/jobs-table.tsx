import { clsx } from 'clsx'
import { AgentLabel } from '@/components/agent-label'
import { JobHypothesis } from '@/components/job-hypothesis'
import { Badge } from '@/components/ui/badge'
import { EmptyState } from '@/components/ui/empty-state'
import { TextLink } from '@/components/ui/link'
import { Section } from '@/components/ui/section'
import { LinkTr, RowAnchor, Table, num } from '@/components/ui/table'
import { formatAccH, formatDate, formatMetric, relTime, shortId } from '@/lib/format'
import { variedEnvByHypothesis, variedEnvSettings } from '@/lib/hypothesis-progress'
import { experimentPath, jobPath } from '@/lib/routes'
import { budgetUnit } from '@/lib/units'
import type { Experiment } from '@/types/api'

const SHOWN = 8

/** The newest jobs, what is running first, and a link to all of them when there are more. */
export function JobsTable({ experimentId, jobs, total, rankingMetric }: {
  experimentId: string
  jobs: Experiment[]
  total: number
  rankingMetric: string | undefined
}) {
  const shown = jobs.slice(0, SHOWN)
  const unit = Array.from(new Set(shown.map(exp => budgetUnit(exp.accelerator_kind)))).join(' / ') || budgetUnit(undefined)
  const variedByHypothesis = variedEnvByHypothesis(shown)
  return (
    <Section
      title="Jobs"
      actions={total > shown.length && <TextLink href={experimentPath(experimentId, 'jobs')} className="text-sm">All {total} jobs →</TextLink>}
      scrollX
    >
      {jobs.length === 0 ? (
        <EmptyState>No jobs submitted yet.</EmptyState>
      ) : (
        <Table className="min-w-[44rem] table-fixed">
          <thead>
            <tr>
              <th className="w-28">Job</th>
              <th title="What the job tests. Hover a row for the settings that differ between these jobs of it">Hypothesis</th>
              <th className="hidden w-32 xl:table-cell">Agent</th>
              <th className="w-36">Status</th>
              <th className="w-28 whitespace-nowrap text-right" title={`The last value the job reported on ${rankingMetric ?? 'the ranking metric'}, which it is ranked by`}>Last metric</th>
              <th className="hidden w-40 whitespace-nowrap text-right lg:table-cell" title="The estimated cost of the job">Cost, {unit}</th>
              <th className="hidden w-28 whitespace-nowrap lg:table-cell">Submitted</th>
            </tr>
          </thead>
          <tbody>
            {shown.map(exp => {
              const cost = exp.estimated_cost_acch != null ? formatAccH(exp.estimated_cost_acch) : '—'
              const varied = variedEnvSettings(exp, variedByHypothesis)
              return (
                <LinkTr key={exp.id} href={jobPath(experimentId, exp.id)}>
                  <td className="whitespace-nowrap font-mono text-2xs" title={exp.id}><RowAnchor>{shortId(exp.id)}</RowAnchor></td>
                  <td className="text-xs" title={[exp.hypothesis, varied].filter(Boolean).join('\n')}>
                    <JobHypothesis job={exp} />
                    {varied && <span className="block truncate font-mono text-2xs text-muted">{varied}</span>}
                  </td>
                  <td className="hidden truncate text-xs xl:table-cell"><AgentLabel id={exp.agent_id} /></td>
                  <td className="truncate" title={exp.eviction_reason}><Badge status={exp.status} /></td>
                  <td className={clsx(num, 'whitespace-nowrap')} title={exp.metric?.name}>{exp.metric ? formatMetric(exp.metric.value) : '—'}</td>
                  <td className={clsx(num, 'hidden whitespace-nowrap lg:table-cell')} title={exp.estimated_cost_acch != null ? `${cost} ${budgetUnit(exp.accelerator_kind)}` : undefined}>{cost}</td>
                  <td className="hidden truncate text-xs text-subtle lg:table-cell" title={formatDate(exp.created_at)}>{relTime(exp.created_at)}</td>
                </LinkTr>
              )
            })}
          </tbody>
        </Table>
      )}
    </Section>
  )
}
