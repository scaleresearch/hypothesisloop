'use client'

import useSWR from 'swr'
import { Link } from '@/components/ui/link'
import { useProjectRouter } from '@/lib/query-state'
import { useRecordScope } from '@/lib/example'
import { fetchHypothesis, fetchPlatformExperiment } from '@/lib/api'
import type { PlatformExperiment, HypothesisLink } from '@/types'
import { Card, CardHeader, CardContent, InsetCard, RowLink } from '@/components/ui/card'
import { PageHeader } from '@/components/ui/page-header'
import { Table } from '@/components/ui/table'
import { Eyebrow } from '@/components/ui/text'
import { Badge, NoSummary } from '@/components/ui/badge'
import { Loading, EmptyState, InlineError } from '@/components/ui/status-message'
import { acceleratorLabel, formatAccH, relTime } from '@/lib/format'
import { PoolAuthor, AddCommentForm, AddLinkForm } from '@/components/human-idea'
import { graphHref, linkGroups } from '@/lib/hypothesis-graph'
import { envSettings, filedAfterVerdict, variedEnvKeys, verdictSetBy } from '@/lib/hypothesis-progress'

function LinkRow({ link, platformExperimentID, pe }: {
  link: HypothesisLink
  platformExperimentID: string
  pe: PlatformExperiment | undefined
}) {
  if (link.hypothesis_id === platformExperimentID) {
    return (
      <RowLink href={`/platform-experiments/${platformExperimentID}`}>
        <Eyebrow>Objective</Eyebrow>
        <span className="min-w-0 flex-1 truncate text-accent">{pe?.name ?? platformExperimentID}</span>
      </RowLink>
    )
  }
  return (
    <RowLink href={`/hypotheses/${link.hypothesis_id}`} title={link.text}>
      <Badge status={link.status}>{link.status}</Badge>
      <span className="min-w-0 flex-1 truncate">{link.text}</span>
      <span className="whitespace-nowrap text-2xs text-subtle">
        linked by <PoolAuthor source={link.author ? 'human' : 'agent'} agentID={link.agent_id} author={link.author} />
      </span>
    </RowLink>
  )
}

export default function HypothesisDetailPage({ params }: { params: { id: string } }) {
  const { id } = params
  const router = useProjectRouter()

  const { data, error, mutate } = useSWR(id, fetchHypothesis, { refreshInterval: 8_000 })

  const { data: pe } = useSWR<PlatformExperiment>(
    data ? ['pe', data.platform_experiment_id] : null,
    () => fetchPlatformExperiment(data!.platform_experiment_id),
  )
  useRecordScope(pe && { project_id: pe.project_id, platform_experiment_id: pe.id })

  if (error) return (
    <div>
      <PageHeader title="Hypothesis Not Found" />
      <p><InlineError>Could not load hypothesis {id}</InlineError></p>
      <Link href="/hypotheses" className="text-sm text-accent hover:underline">← Hypothesis graph</Link>
    </div>
  )

  if (!data) return <Loading />

  const jobs = [...(data.jobs ?? [])].sort((a, b) => a.created_at.localeCompare(b.created_at))
  const findings = data.findings ?? []
  const comments = data.comments ?? []
  const jobByID = new Map(jobs.map(j => [j.id, j]))
  const summarized = new Set(findings.map(f => f.experiment_id))
  const sinceVerdict = findings.filter(f => filedAfterVerdict(data, f.created_at)).length
  // With nothing varied, every setting is shown.
  const variedEnv = variedEnvKeys(jobs)
  const shownEnv = variedEnv.length > 0 ? variedEnv : Array.from(new Set(jobs.flatMap(j => Object.keys(j.job?.env ?? {})))).sort()
  const metricName = jobs.find(j => j.metric)?.metric?.name

  return (
    <div>
      <PageHeader
        back={{
          label: pe?.name ?? data.platform_experiment_id.slice(0, 12) + '…',
          onClick: () => router.push(`/platform-experiments/${data.platform_experiment_id}`),
        }}
        title={
          <>
            <span className="font-mono text-base font-normal text-subtle">{data.id}</span>
            <Badge status={data.status ?? 'open'}>{data.status ?? 'open'}</Badge>
          </>
        }
        description={
          <span className="font-mono text-2xs">
            Registered by <PoolAuthor source={data.source} agentID={data.agent_id} author={data.author} /> · {relTime(data.created_at)}
            {data.status_changed_at && <> · {data.status} {verdictSetBy(data)}</>}
            {sinceVerdict > 0 && (
              <span className="text-warning"> · {sinceVerdict} {sinceVerdict === 1 ? 'finding' : 'findings'} filed since</span>
            )}
          </span>
        }
        actions={
          <Link href={graphHref(data.platform_experiment_id, data.id)} className="text-xs text-accent hover:underline">
            View in graph →
          </Link>
        }
      />

      <Card>
        <CardHeader>Hypothesis</CardHeader>
        <CardContent>
          <p className="max-w-[760px] whitespace-pre-wrap break-words text-base leading-relaxed">
            {data.text}
          </p>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>Links</CardHeader>
        <CardContent>
          <div className="grid grid-cols-[minmax(0,1fr)] max-w-[760px] gap-3">
            {linkGroups(data.links.out, data.links.in).map(group => (
              <div key={group.label} className="grid grid-cols-[minmax(0,1fr)] gap-0.5">
                <Eyebrow>{group.label}</Eyebrow>
                {group.items.map(l => (
                  <LinkRow
                    key={`${l.type}:${l.hypothesis_id}`}
                    link={l}
                    platformExperimentID={data.platform_experiment_id}
                    pe={pe}
                  />
                ))}
              </div>
            ))}
          </div>
          <AddLinkForm hypothesis={data} onAdded={() => mutate()} />
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          Findings
          {data.finding_count > 0 && (
            <span className="ml-2 text-xs font-normal text-muted">
              ({data.finding_count}{data.finding_count > findings.length ? `, showing ${findings.length}` : ''})
            </span>
          )}
        </CardHeader>
        <CardContent>
          {findings.length === 0 ? (
            <EmptyState>
              No findings filed yet. Agents write a summary via POST /experiments/{'{id}'}/summary once a job testing
              this hypothesis reaches a terminal state.
            </EmptyState>
          ) : (
            <div className="grid gap-2.5">
              {findings.map(f => {
                const job = jobByID.get(f.experiment_id)
                return (
                  <InsetCard key={f.id}>
                    <div className="mb-1.5 flex items-center justify-between text-2xs">
                      <span className="flex items-center gap-2 font-mono">
                        <span className="font-semibold">{f.agent_id}</span>
                        <Link href={`/jobs/${f.experiment_id}`} className="text-accent hover:underline">{f.experiment_id.slice(0, 8)}…</Link>
                        {job && <Badge status={job.status}>{job.status}</Badge>}
                        {job?.metric && <span className="text-accent" title={`This job's best ${job.metric.name}`}>{job.metric.best.toFixed(4)}</span>}
                      </span>
                      <span className="font-mono text-subtle">
                        {filedAfterVerdict(data, f.created_at) && <span className="text-warning">after the verdict · </span>}
                        {relTime(f.created_at)}
                      </span>
                    </div>
                    <p className="whitespace-pre-wrap text-sm">{f.summary}</p>
                  </InsetCard>
                )
              })}
            </div>
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          Comments
          {data.comment_count > 0 && (
            <span className="ml-2 text-xs font-normal text-muted">
              ({data.comment_count}{data.comment_count > comments.length ? `, showing ${comments.length}` : ''})
            </span>
          )}
        </CardHeader>
        <CardContent>
          {comments.length === 0 ? (
            <EmptyState>No comments yet. A note here needs no job behind it.</EmptyState>
          ) : (
            <div className="grid gap-2.5">
              {comments.map(c => (
                <InsetCard key={c.id}>
                  <div className="mb-1.5 flex items-center justify-between text-2xs">
                    <PoolAuthor source={c.source} agentID={c.agent_id} author={c.author} />
                    <span className="font-mono text-subtle">{relTime(c.created_at)}</span>
                  </div>
                  <p className="whitespace-pre-wrap text-sm">{c.text}</p>
                </InsetCard>
              ))}
            </div>
          )}
          <AddCommentForm hypothesisID={data.id} onAdded={() => mutate()} />
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          Jobs testing this hypothesis
          {data.job_count > 0 && (
            <span className="ml-2 text-xs font-normal text-muted">
              ({data.job_count}{data.job_count > jobs.length ? `, showing ${jobs.length}` : ''})
            </span>
          )}
        </CardHeader>
        <CardContent scrollX>
          <Table>
            <thead>
              <tr>
                <th>Job ID</th>
                <th>Agent</th>
                <th>Status</th>
                <th>{variedEnv.length > 0 ? 'Varied settings' : 'Settings'}</th>
                <th>Accelerator</th>
                <th className="text-right">Est. Cost</th>
                <th className="text-right">Best {metricName ?? 'metric'}</th>
                <th>Submitted</th>
              </tr>
            </thead>
            <tbody>
              {jobs.length === 0 ? (
                <tr>
                  <td colSpan={8}>
                    <EmptyState>No jobs have tested this hypothesis yet.</EmptyState>
                  </td>
                </tr>
              ) : jobs.map(job => {
                const j = job
                const status = j.status ?? 'UNKNOWN'
                const cost = j.estimated_cost_acch != null ? formatAccH(j.estimated_cost_acch) : null
                return (
                  <tr
                    key={job.id}
                    tabIndex={0}
                    role="link"
                    className="cursor-pointer"
                    onClick={() => router.push(`/jobs/${job.id}`)}
                    onKeyDown={e => { if (e.key === 'Enter') router.push(`/jobs/${job.id}`) }}
                  >
                    <td className="font-mono text-2xs text-subtle">{job.id.slice(0, 8)}…</td>
                    <td className="font-mono text-xs">{job.agent_id}</td>
                    <td className="whitespace-nowrap">
                      <Badge status={status}>{status}</Badge>
                      {status === 'COMPLETED' && data.finding_count === findings.length && !summarized.has(job.id) && <NoSummary className="ml-1.5" />}
                    </td>
                    <td className="font-mono text-2xs">
                      {envSettings(j, shownEnv) || '—'}
                    </td>
                    <td className="font-mono text-2xs">{j.accelerator_count}× {acceleratorLabel(j.accelerator_type)}</td>
                    <td className="text-right font-mono text-2xs">
                      {cost != null ? `${cost} AccH` : '—'}
                    </td>
                    <td className="text-right font-mono text-xs">
                      {j.metric ? <span className="text-accent">{j.metric.best.toFixed(4)}</span> : <span className="text-muted">—</span>}
                    </td>
                    <td className="whitespace-nowrap font-mono text-2xs text-subtle">
                      {relTime(j.submitted_at ?? j.created_at)}
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </Table>
        </CardContent>
      </Card>
    </div>
  )
}
