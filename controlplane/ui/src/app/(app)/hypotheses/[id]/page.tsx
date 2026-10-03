'use client'

import { useState } from 'react'
import useSWR from 'swr'
import { clsx } from 'clsx'
import { TextLink } from '@/components/ui/link'
import { useProjectRouter } from '@/lib/query-state'
import { ReadOnlyProvider, useExperimentAccess, useRecordScope } from '@/lib/read-only'
import { fetchHypothesis } from '@/lib/api'
import type { PlatformExperiment, HypothesisLink } from '@/types'
import { Card, CardHeader, CardContent, CardTitle, InsetCard, RowLink } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { CopyButton } from '@/components/ui/copy-button'
import { PageHeader } from '@/components/ui/page-header'
import { LinkTr, Table, num, quiet } from '@/components/ui/table'
import { Eyebrow } from '@/components/ui/text'
import { Badge, NoSummary } from '@/components/ui/badge'
import { Loading, EmptyState, InlineError } from '@/components/ui/status-message'
import { acceleratorLabel, formatAccH, relTime } from '@/lib/format'
import { budgetUnit, kindOf } from '@/lib/units'
import { AddCommentForm, AddLinkForm } from '@/components/human-idea'
import { Actor, Verdict } from '@/components/actor'
import { AgentLabel, AgentNames } from '@/components/agent-label'
import { Branch, Commit } from '@/components/code-ref'
import { graphHref, linkGroups } from '@/lib/hypothesis-graph'
import { envSettings, filedAfterVerdict, variedEnvKeys } from '@/lib/hypothesis-progress'

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
        linked by <Actor actor={link.actor} agentID={link.agent_id} />
      </span>
    </RowLink>
  )
}

// Text longer than this is cut to three lines until asked for in full.
const LONG_TEXT = 240

/** The hypothesis as the page's title: all of it, the long ones on request. */
function HypothesisText({ text }: { text: string }) {
  const [open, setOpen] = useState(false)
  const long = text.length > LONG_TEXT
  return (
    <span className="min-w-0">
      <span className={clsx('block whitespace-pre-wrap text-lg font-semibold leading-snug', long && !open && 'line-clamp-3')}>{text}</span>
      {long && <Button variant="link" size="sm" onClick={() => setOpen(!open)}>{open ? 'Show less' : 'Show all of it'}</Button>}
    </span>
  )
}

export default function HypothesisDetailPage({ params }: { params: { id: string } }) {
  const { id } = params
  const router = useProjectRouter()

  const { data, error, mutate } = useSWR(id, fetchHypothesis, { refreshInterval: 8_000 })

  const access = useExperimentAccess(data?.platform_experiment_id)
  const pe = access.pe
  useRecordScope(pe?.project_id)

  if (error) return (
    <div>
      <PageHeader title="Hypothesis" />
      <p><InlineError>Could not load hypothesis {id}: {error.message}</InlineError></p>
      <TextLink href="/hypotheses" className="text-sm">← Hypothesis graph</TextLink>
    </div>
  )

  if (!data) return <Loading />

  const jobs = [...(data.jobs ?? [])].sort((a, b) => a.created_at.localeCompare(b.created_at))
  const findings = data.findings ?? []
  const comments = data.comments ?? []
  const jobByID = new Map(jobs.map(j => [j.id, j]))
  const sinceVerdict = findings.filter(f => filedAfterVerdict(data, f.created_at)).length
  // With nothing varied, every setting is shown.
  const variedEnv = variedEnvKeys(jobs)
  const shownEnv = variedEnv.length > 0 ? variedEnv : Array.from(new Set(jobs.flatMap(j => Object.keys(j.job?.env ?? {})))).sort()
  const metricName = jobs.find(j => j.metric)?.metric?.name

  return (
    <ReadOnlyProvider value={access.readOnly}>
    <AgentNames platformExperimentID={data.platform_experiment_id}>
    <div>
      <PageHeader
        back={{
          label: pe?.name ?? data.platform_experiment_id.slice(0, 12) + '…',
          onClick: () => router.push(`/platform-experiments/${data.platform_experiment_id}`),
        }}
        title={
          <>
            <HypothesisText text={data.text} />
            <Badge status={data.status ?? 'open'}>{data.status ?? 'open'}</Badge>
          </>
        }
        description={
          <span className="flex flex-wrap items-center gap-x-2 gap-y-1 text-2xs">
            <span>
              Registered by <Actor actor={data.actor} agentID={data.agent_id} /> · {relTime(data.created_at)}
              {data.status_changed_at && <> · <Verdict hypothesis={data} /></>}
              {sinceVerdict > 0 && (
                <span className="text-warning"> · {sinceVerdict} {sinceVerdict === 1 ? 'finding' : 'findings'} filed since</span>
              )}
            </span>
            <span className="font-mono">· {data.id}</span>
            <CopyButton value={data.id} />
          </span>
        }
        actions={
          <TextLink href={graphHref(data.platform_experiment_id, data.id)} className="text-xs">
            View in graph →
          </TextLink>
        }
      />

      <Card>
        <CardHeader><CardTitle>Links</CardTitle></CardHeader>
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
          <CardTitle note={data.base_ref && <>a new one starts from <span className="font-mono"><Commit codeRef={data.base_ref} /></span></>}>Implementations</CardTitle>
        </CardHeader>
        <CardContent scrollX>
          {data.implementations.length === 0 ? (
            <EmptyState>No agent has run this hypothesis yet. Each one that does works on a branch of its own.</EmptyState>
          ) : (
            <Table>
              <thead>
                <tr>
                  <th>Agent</th>
                  <th>Branch</th>
                  <th>Newest commit run</th>
                </tr>
              </thead>
              <tbody>
                {data.implementations.map(i => (
                  <tr key={`${i.agent_id}:${i.branch}`}>
                    <td className="text-xs"><AgentLabel id={i.agent_id} /></td>
                    <td className="font-mono text-xs"><Branch branch={i.branch} codeRef={i.code_ref} platformExperimentID={data.platform_experiment_id} /></td>
                    <td className="font-mono text-xs"><Commit codeRef={i.code_ref} /></td>
                  </tr>
                ))}
              </tbody>
            </Table>
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle note={data.finding_count > 0 && `${data.finding_count}${data.finding_count > findings.length ? `, showing ${findings.length}` : ''}`}>Findings</CardTitle>
        </CardHeader>
        <CardContent>
          {findings.length === 0 ? (
            <EmptyState>
              No findings yet. An agent files one as the summary of a job that tested this hypothesis, once the job
              has ended.
            </EmptyState>
          ) : (
            <div className="grid gap-2.5">
              {findings.map(f => {
                const job = jobByID.get(f.experiment_id)
                return (
                  <InsetCard key={f.id}>
                    <div className="mb-1.5 flex items-center justify-between text-2xs">
                      <span className="flex items-center gap-2 font-mono">
                        <span className="font-semibold"><Actor actor={f.actor} agentID={f.agent_id} /></span>
                        <TextLink href={`/jobs/${f.experiment_id}`}>{f.experiment_id.slice(0, 8)}…</TextLink>
                        {job && <Badge status={job.status}>{job.status}</Badge>}
                        {job?.metric && <span title={`The last ${job.metric.name} this job reported`}>{job.metric.value.toFixed(4)}</span>}
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
          <CardTitle note={data.comment_count > 0 && `${data.comment_count}${data.comment_count > comments.length ? `, showing ${comments.length}` : ''}`}>Comments</CardTitle>
        </CardHeader>
        <CardContent>
          {comments.length === 0 ? (
            <EmptyState>No comments yet. A note here needs no job behind it.</EmptyState>
          ) : (
            <div className="grid gap-2.5">
              {comments.map(c => (
                <InsetCard key={c.id}>
                  <div className="mb-1.5 flex items-center justify-between text-2xs">
                    <Actor actor={c.actor} agentID={c.agent_id} />
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
          <CardTitle note={data.job_count > 0 && `${data.job_count}${data.job_count > jobs.length ? `, showing ${jobs.length}` : ''}`}>Jobs testing this hypothesis</CardTitle>
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
                <th className="text-right">Est. cost</th>
                <th className="text-right">Last {metricName ?? 'metric'}</th>
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
                  <LinkTr key={job.id} href={`/jobs/${job.id}`}>
                    <td className={quiet}>{job.id.slice(0, 8)}…</td>
                    <td className="text-xs" title={job.agent_id}>{job.agent_name}</td>
                    <td className="whitespace-nowrap">
                      <Badge status={status}>{status}</Badge>
                      {job.summary_due && <NoSummary className="ml-1.5" />}
                    </td>
                    <td className="font-mono text-2xs">
                      {envSettings(j, shownEnv) || '—'}
                    </td>
                    <td className="font-mono text-2xs">{j.accelerator_count}× {acceleratorLabel(j.accelerator_type)}</td>
                    <td className={num}>
                      {cost != null ? `${cost} ${budgetUnit(kindOf(j.accelerator_type))}` : '—'}
                    </td>
                    <td className={num}>
                      {j.metric ? j.metric.value.toFixed(4) : <span className="text-muted">—</span>}
                    </td>
                    <td className={quiet}>{relTime(j.submitted_at ?? j.created_at)}</td>
                  </LinkTr>
                )
              })}
            </tbody>
          </Table>
        </CardContent>
      </Card>
    </div>
    </AgentNames>
    </ReadOnlyProvider>
  )
}
