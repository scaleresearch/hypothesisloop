'use client'

import { useState } from 'react'
import useSWR from 'swr'
import { clsx } from 'clsx'
import { TextLink } from '@/components/ui/link'
import { useRouter } from 'next/navigation'
import { useExperimentAccess } from '@/lib/experiment-actions'
import { experimentPath, hypothesisPath, jobPath } from '@/lib/routes'
import { fetchHypothesis } from '@/lib/api/hypotheses'
import { InsetCard, RowLink } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { CopyButton } from '@/components/ui/copy-button'
import { EmptyState } from '@/components/ui/empty-state'
import { Page } from '@/components/ui/page'
import { Section } from '@/components/ui/section'
import { Inline, Stack } from '@/components/ui/stack'
import { LinkTr, RowAnchor, Table, num, quiet } from '@/components/ui/table'
import { Code, Eyebrow, Text } from '@/components/ui/text'
import { Badge } from '@/components/ui/badge'
import { Loading, ErrorMessage } from '@/components/ui/status-message'
import { acceleratorLabel, formatAccH, relTime, shortId } from '@/lib/format'
import { budgetUnit } from '@/lib/units'
import { AddCommentForm, AddLinkForm } from '@/components/hypotheses/human-idea'
import { Actor, Verdict } from '@/components/actor'
import { AgentLabel, AgentNames } from '@/components/agent-label'
import { Branch, Commit } from '@/components/code-ref'
import { graphHref, linkGroups } from '@/lib/hypothesis-graph'
import { envSettings, filedAfterVerdict, variedEnvKeys } from '@/lib/hypothesis-progress'
import { keys } from '@/lib/keys'
import type { Link, PlatformExperiment } from '@/types/api'

function LinkRow({ link, platformExperimentID, pe }: {
  link: Link
  platformExperimentID: string
  pe: PlatformExperiment | undefined
}) {
  if (link.hypothesis_id === platformExperimentID) {
    return (
      <RowLink href={experimentPath(platformExperimentID)}>
        <Eyebrow>Objective</Eyebrow>
        <span className="min-w-0 flex-1 truncate text-accent">{pe?.name ?? platformExperimentID}</span>
      </RowLink>
    )
  }
  return (
    <RowLink href={hypothesisPath(platformExperimentID, link.hypothesis_id)} title={link.text}>
      <Badge status={link.status}>{link.status}</Badge>
      <span className="min-w-0 flex-1 truncate">{link.text}</span>
      <Text as="span" variant="meta" tone="subtle" className="whitespace-nowrap">
        linked by <Actor actor={link.actor} agentID={link.agent_id} />
      </Text>
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
      <span className={clsx('block whitespace-pre-wrap text-lg leading-snug', long && !open && 'line-clamp-3')}>{text}</span>
      {long && <Button variant="link" size="sm" onClick={() => setOpen(!open)}>{open ? 'Show less' : 'Show all of it'}</Button>}
    </span>
  )
}

export default function HypothesisDetailPage({ params }: { params: { experimentId: string; hypothesisId: string } }) {
  const { experimentId, hypothesisId: id } = params
  const router = useRouter()

  const { data, error, mutate } = useSWR(keys.hypothesis(id), () => fetchHypothesis(id))

  const access = useExperimentAccess(experimentId)
  const pe = access.pe

  // A hypothesis is reached through the experiment it belongs to, and through no other.
  const elsewhere = data && data.platform_experiment_id !== experimentId
  if (error || elsewhere) return (
    <Page title="Hypothesis">
      <ErrorMessage>Could not load hypothesis {id}: {elsewhere ? 'it is not in this experiment' : error.message}</ErrorMessage>
      <TextLink href={graphHref(experimentId)} className="text-sm">← Hypothesis graph</TextLink>
    </Page>
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
    <AgentNames platformExperimentID={data.platform_experiment_id}>
    <Page
        back={{
          label: pe?.name ?? data.platform_experiment_id.slice(0, 12) + '…',
          onClick: () => router.push(experimentPath(experimentId)),
        }}
        title={
          <>
            <HypothesisText text={data.text} />
            <Badge status={data.status ?? 'open'}>{data.status ?? 'open'}</Badge>
          </>
        }
        description={
          <Inline className="text-2xs">
            <span>
              Registered by <Actor actor={data.actor} agentID={data.agent_id} /> · {relTime(data.created_at)}
              {data.status_changed_at && <> · <Verdict hypothesis={data} /></>}
              {sinceVerdict > 0 && (
                <span className="text-warning"> · {sinceVerdict} {sinceVerdict === 1 ? 'finding' : 'findings'} filed since</span>
              )}
            </span>
            <Code>· {data.id}</Code>
            <CopyButton value={data.id} />
          </Inline>
        }
        actions={
          <TextLink href={graphHref(data.platform_experiment_id, data.id)} className="text-xs">
            View in graph →
          </TextLink>
        }
    >
      <Section title="Links">
          <Stack className="max-w-3xl">
            {linkGroups(data.links.out, data.links.in).map(group => (
              <Stack key={group.label} gap="2xs">
                <Eyebrow>{group.label}</Eyebrow>
                {group.items.map(l => (
                  <LinkRow
                    key={`${l.type}:${l.hypothesis_id}`}
                    link={l}
                    platformExperimentID={data.platform_experiment_id}
                    pe={pe}
                  />
                ))}
              </Stack>
            ))}
          </Stack>
          <AddLinkForm hypothesis={data} onAdded={() => mutate()} />
      </Section>

      <Section title="Implementations" note={data.base_ref && <>a new one starts from <Code><Commit codeRef={data.base_ref} /></Code></>} scrollX>
          {data.implementations.length === 0 ? (
            <EmptyState>No agent has run this hypothesis yet.</EmptyState>
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
      </Section>

      <Section title="Findings" note={data.finding_count > 0 && `${data.finding_count}${data.finding_count > findings.length ? `, showing ${findings.length}` : ''}`}>
          {findings.length === 0 ? (
            <EmptyState>No findings yet.</EmptyState>
          ) : (
            <Stack>
              {findings.map(f => {
                const job = jobByID.get(f.experiment_id)
                return (
                  <InsetCard key={f.id}>
                    <div className="flex items-center justify-between text-2xs">
                      <span className="flex items-center gap-2 font-mono">
                        <span className="font-medium"><Actor actor={f.actor} agentID={f.agent_id} /></span>
                        <TextLink href={jobPath(experimentId, f.experiment_id)}>{shortId(f.experiment_id)}</TextLink>
                        {job && <Badge status={job.status}>{job.status}</Badge>}
                        {job?.metric && <span title={`The last ${job.metric.name} this job reported`}>{job.metric.value.toFixed(4)}</span>}
                      </span>
                      <Code className="text-subtle">
                        {filedAfterVerdict(data, f.created_at) && <span className="text-warning">after the verdict · </span>}
                        {relTime(f.created_at)}
                      </Code>
                    </div>
                    <Text pre>{f.summary}</Text>
                  </InsetCard>
                )
              })}
            </Stack>
          )}
      </Section>

      <Section title="Comments" note={data.comment_count > 0 && `${data.comment_count}${data.comment_count > comments.length ? `, showing ${comments.length}` : ''}`}>
          {comments.length === 0 ? (
            <EmptyState>No comments yet.</EmptyState>
          ) : (
            <Stack>
              {comments.map(c => (
                <InsetCard key={c.id}>
                  <div className="flex items-center justify-between text-2xs">
                    <Actor actor={c.actor} agentID={c.agent_id} />
                    <Code className="text-subtle">{relTime(c.created_at)}</Code>
                  </div>
                  <Text pre>{c.text}</Text>
                </InsetCard>
              ))}
            </Stack>
          )}
          <AddCommentForm hypothesis={data} onAdded={() => mutate()} />
      </Section>

      <Section title="Jobs testing this hypothesis" note={data.job_count > 0 && `${data.job_count}${data.job_count > jobs.length ? `, showing ${jobs.length}` : ''}`} scrollX>
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
                  <LinkTr key={job.id} href={jobPath(experimentId, job.id)}>
                    <td className={quiet}><RowAnchor>{shortId(job.id)}</RowAnchor></td>
                    <td className="text-xs" title={job.agent_id}>{job.agent_name}</td>
                    <td className="whitespace-nowrap">
                      <Badge status={status}>{status}</Badge>
                    </td>
                    <td className="font-mono text-2xs">
                      {envSettings(j, shownEnv) || '—'}
                    </td>
                    <td className="font-mono text-2xs">{j.accelerator_count}× {acceleratorLabel(j.accelerator_type)}</td>
                    <td className={num}>
                      {cost != null ? `${cost} ${budgetUnit(j.accelerator_kind)}` : '—'}
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
      </Section>
    </Page>
    </AgentNames>
  )
}
