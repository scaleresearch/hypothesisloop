import { clsx } from 'clsx'
import { AgentLabel } from '@/components/agent-label'
import { JobHypothesis } from '@/components/job-hypothesis'
import { Badge } from '@/components/ui/badge'
import { EmptyState } from '@/components/ui/empty-state'
import { TextLink } from '@/components/ui/link'
import { Section } from '@/components/ui/section'
import { LinkTr, RowAnchor, Table, num } from '@/components/ui/table'
import { formatAccH, relTime, shortId } from '@/lib/format'
import { variedEnvByHypothesis, variedEnvSettings } from '@/lib/hypothesis-progress'
import { experimentPath, jobPath } from '@/lib/routes'
import { budgetUnit, kindOf } from '@/lib/units'
import type { Experiment } from '@/types/api'

/** The jobs shown, what is running first, and a link to all of them when there are more. */
export function JobsTable({ experimentId, jobs, total, rankingMetric }: {
  experimentId: string
  jobs: Experiment[]
  total: number
  rankingMetric: string | undefined
}) {
  const variedByHypothesis = variedEnvByHypothesis(jobs)
  return (
    <Section title="Jobs" scrollX>
      {jobs.length === 0 ? (
        <EmptyState>No jobs submitted yet.</EmptyState>
      ) : (
        <Table className="min-w-[44rem] table-fixed">
          <thead>
            <tr>
              <th className="w-24">Job</th>
              <th className="hidden w-32 xl:table-cell">Agent</th>
              <th className="w-36">Status</th>
              <th title="What the job tests. Hover a row for the settings that differ between these jobs of it">Hypothesis</th>
              <th className="w-24 text-right" title={`The last value the job reported on ${rankingMetric ?? 'the ranking metric'}, which it is ranked by`}>Last metric</th>
              <th className="w-32 text-right">Est. cost</th>
              <th className="hidden w-28 xl:table-cell">Submitted</th>
            </tr>
          </thead>
          <tbody>
            {jobs.map(exp => {
              const cost = exp.estimated_cost_acch != null ? `${formatAccH(exp.estimated_cost_acch)} ${budgetUnit(kindOf(exp.accelerator_type))}` : '—'
              const varied = variedEnvSettings(exp, variedByHypothesis)
              return (
                <LinkTr key={exp.id} href={jobPath(experimentId, exp.id)}>
                  <td className="whitespace-nowrap font-mono text-2xs" title={exp.id}><RowAnchor>{shortId(exp.id)}</RowAnchor></td>
                  <td className="hidden truncate text-xs xl:table-cell"><AgentLabel id={exp.agent_id} /></td>
                  <td className="truncate" title={exp.eviction_reason}><Badge status={exp.status} /></td>
                  <td className="truncate text-xs" title={[exp.hypothesis, varied].filter(Boolean).join('\n')}>
                    <JobHypothesis job={exp} />
                    {varied && <span className="ml-2 font-mono text-2xs text-subtle">{varied}</span>}
                  </td>
                  <td className={clsx(num, 'truncate')} title={exp.metric?.name}>{exp.metric?.value.toFixed(4) ?? '—'}</td>
                  <td className={clsx(num, 'truncate')} title={cost}>{cost}</td>
                  <td className="hidden truncate text-xs text-subtle xl:table-cell" title={new Date(exp.created_at).toLocaleString()}>{relTime(exp.created_at)}</td>
                </LinkTr>
              )
            })}
          </tbody>
        </Table>
      )}
      {total > jobs.length && (
        <TextLink href={experimentPath(experimentId, 'jobs')} className="mt-2 inline-block text-sm">
          All {total} jobs →
        </TextLink>
      )}
    </Section>
  )
}
