'use client'

import useSWR from 'swr'
import Link from 'next/link'
import { useRouter } from 'next/navigation'
import { fetchHypothesis, fetchPlatformExperiment, fetchExperimentMetrics } from '@/lib/api'
import type { PlatformExperiment, MetricDefinition, HypothesisLink } from '@/types'
import { Pod, PodHeader, PodContent } from '@/components/ui/pod'
import { Badge, TierBadge } from '@/components/ui/badge'
import { Loading, EmptyState } from '@/components/ui/status-message'
import { acceleratorLabel, formatAccH, relTime } from '@/lib/format'
import { PoolAuthor, AddCommentForm, AddLinkForm } from '@/components/human-idea'
import { graphHref, linkGroups } from '@/lib/hypothesis-graph'

// The registry's job record has no final_metric_value field at all (confirmed against the live
// API — it was always undefined, rendering "—" for every job regardless of outcome). The real
// per-job metric history lives in the metrics store, keyed by metric_name — this fetches it for
// one job and reduces to the platform experiment's primary metric's best-known value (max for
// "maximize", min for "minimize"), matching how the platform-experiment scoreboard derives a
// job's result rather than trusting a job-record field that was never populated.
function JobFinalMetric({ jobId, primaryMetric }: { jobId: string; primaryMetric?: MetricDefinition }) {
  const { data } = useSWR(
    primaryMetric ? ['job-metrics', jobId] : null,
    () => fetchExperimentMetrics(jobId),
    { refreshInterval: 15_000 },
  )
  if (!primaryMetric) return <span className="text-muted">—</span>
  const values = (data ?? [])
    .filter(p => p.metric_name === primaryMetric.key)
    .map(p => p.metric_value)
  if (values.length === 0) return <span className="text-muted">—</span>
  const best = primaryMetric.direction === 'maximize' ? Math.max(...values) : Math.min(...values)
  return <span className="accent">{best.toFixed(4)}</span>
}

function LinkRow({ link, platformExperimentID, pe }: {
  link: HypothesisLink
  platformExperimentID: string
  pe: PlatformExperiment | undefined
}) {
  if (link.hypothesis_id === platformExperimentID) {
    return (
      <Link href={`/platform-experiments/${platformExperimentID}`} className="hg-hit text-link">
        <span className="uppercase-label">Objective</span>
        <span className="hg-outline-text">{pe?.name ?? platformExperimentID}</span>
      </Link>
    )
  }
  return (
    <Link href={`/hypotheses/${link.hypothesis_id}`} className="hg-hit" title={link.text}>
      <Badge status={link.status}>{link.status}</Badge>
      <span className="hg-outline-text">{link.text}</span>
      <span className="text-dim" style={{ fontSize: 11, whiteSpace: 'nowrap' }}>
        linked by <PoolAuthor source={link.author ? 'human' : 'agent'} agentID={link.agent_id} author={link.author} />
      </span>
    </Link>
  )
}

export default function HypothesisDetailPage({ params }: { params: { id: string } }) {
  const { id } = params
  const router = useRouter()

  const { data, error, mutate } = useSWR(id, fetchHypothesis, { refreshInterval: 8_000 })

  const { data: pe } = useSWR<PlatformExperiment>(
    data ? ['pe', data.platform_experiment_id] : null,
    () => fetchPlatformExperiment(data!.platform_experiment_id),
  )

  if (error) return (
    <div>
      <div className="wa-title"><h1>Hypothesis Not Found</h1></div>
      <p className="text-error">Could not load hypothesis {id}</p>
      <Link href="/hypotheses" className="text-link" style={{ fontSize: 13 }}>← Hypothesis graph</Link>
    </div>
  )

  if (!data) return <Loading />

  const jobs = data.jobs ?? []
  const findings = data.findings ?? []
  const comments = data.comments ?? []
  const jobByID = new Map(jobs.map(j => [j.id, j]))
  const primaryMetric = pe?.metrics?.[0]

  return (
    <div>
      <div className="wa-title" style={{ display: 'flex', alignItems: 'flex-end', gap: 16 }}>
        <div style={{ flex: 1 }}>
          <div
            className="text-dim"
            style={{ marginBottom: 6, cursor: 'pointer', fontSize: 12 }}
            onClick={() => router.push(`/platform-experiments/${data.platform_experiment_id}`)}
          >
            ← {pe?.name ?? data.platform_experiment_id.slice(0, 12) + '…'}
          </div>
          <h1 style={{ display: 'flex', alignItems: 'center', gap: 10, flexWrap: 'wrap', wordBreak: 'break-word' }}>
            <span className="mono text-dim" style={{ fontSize: 14 }}>{data.id.slice(0, 8)}…</span>
            <Badge status={data.status ?? 'open'}>{data.status ?? 'open'}</Badge>
          </h1>
          <p className="text-muted mono" style={{ fontSize: 11, marginTop: 4 }}>
            Registered by <PoolAuthor source={data.source} agentID={data.agent_id} author={data.author} /> · {relTime(data.created_at)}
            {data.status_changed_at && <> · {data.status} {relTime(data.status_changed_at)}</>}
          </p>
        </div>
        <Link href={graphHref(data.platform_experiment_id, data.id)} className="text-link" style={{ fontSize: 12, marginBottom: 4 }}>
          View in graph →
        </Link>
      </div>

      <Pod style={{ marginBottom: 12 }}>
        <PodHeader>Hypothesis</PodHeader>
        <PodContent>
          <p
            style={{
              maxWidth: 760,
              fontSize: 14,
              lineHeight: 1.65,
              whiteSpace: 'pre-wrap',
              overflowWrap: 'break-word',
              wordBreak: 'break-word',
            }}
          >
            {data.text}
          </p>
        </PodContent>
      </Pod>

      <Pod>
        <PodHeader>Links</PodHeader>
        <PodContent>
          <div className="hg-stack" style={{ gap: 12, maxWidth: 760 }}>
            {linkGroups(data.links.out, data.links.in).map(group => (
              <div key={group.label} className="hg-stack" style={{ gap: 2 }}>
                <span className="uppercase-label">{group.label}</span>
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
        </PodContent>
      </Pod>

      <Pod>
        <PodHeader>
          Findings
          {data.finding_count > 0 && (
            <span className="text-muted" style={{ fontWeight: 400, marginLeft: 8 }}>
              ({data.finding_count}{data.finding_count > findings.length ? `, showing ${findings.length}` : ''})
            </span>
          )}
        </PodHeader>
        <PodContent>
          {findings.length === 0 ? (
            <EmptyState>
              No findings filed yet. Agents write a summary via POST /experiments/{'{id}'}/summary once a job testing
              this hypothesis reaches a terminal state.
            </EmptyState>
          ) : (
            <div style={{ display: 'grid', gap: 10 }}>
              {findings.map(f => {
                const job = jobByID.get(f.experiment_id)
                return (
                  <div key={f.id} className="wa-mini-card">
                    <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 6, fontSize: 11 }}>
                      <span style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
                        <span className="mono" style={{ fontWeight: 600 }}>{f.agent_id}</span>
                        <Link href={`/jobs/${f.experiment_id}`} className="mono text-link">{f.experiment_id.slice(0, 8)}…</Link>
                        {job && <Badge status={job.status}>{job.status}</Badge>}
                      </span>
                      <span className="text-dim mono">{relTime(f.created_at)}</span>
                    </div>
                    <p style={{ fontSize: 13, whiteSpace: 'pre-wrap' }}>{f.summary}</p>
                  </div>
                )
              })}
            </div>
          )}
        </PodContent>
      </Pod>

      <Pod>
        <PodHeader>
          Comments
          {data.comment_count > 0 && (
            <span className="text-muted" style={{ fontWeight: 400, marginLeft: 8 }}>
              ({data.comment_count}{data.comment_count > comments.length ? `, showing ${comments.length}` : ''})
            </span>
          )}
        </PodHeader>
        <PodContent>
          {comments.length === 0 ? (
            <EmptyState>No comments yet. A note here needs no job behind it.</EmptyState>
          ) : (
            <div style={{ display: 'grid', gap: 10 }}>
              {comments.map(c => (
                <div key={c.id} className="wa-mini-card">
                  <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 6, fontSize: 11 }}>
                    <PoolAuthor source={c.source} agentID={c.agent_id} author={c.author} />
                    <span className="text-dim mono">{relTime(c.created_at)}</span>
                  </div>
                  <p style={{ fontSize: 13, whiteSpace: 'pre-wrap' }}>{c.text}</p>
                </div>
              ))}
            </div>
          )}
          <AddCommentForm hypothesisID={data.id} onAdded={() => mutate()} />
        </PodContent>
      </Pod>

      <Pod>
        <PodHeader>
          Jobs testing this hypothesis
          {data.job_count > 0 && (
            <span className="text-muted" style={{ fontWeight: 400, marginLeft: 8 }}>
              ({data.job_count}{data.job_count > jobs.length ? `, showing ${jobs.length}` : ''})
            </span>
          )}
        </PodHeader>
        <PodContent scrollX>
          <table className="wa-table">
            <thead>
              <tr>
                <th>Job ID</th>
                <th>Agent</th>
                <th>Status</th>
                <th>Tier</th>
                <th>Accelerator</th>
                <th style={{ textAlign: 'right' }}>Est. Cost</th>
                <th style={{ textAlign: 'right' }}>Best {primaryMetric?.key ?? 'Metric'}</th>
                <th>Submitted</th>
              </tr>
            </thead>
            <tbody>
              {jobs.length === 0 ? (
                <tr>
                  <td colSpan={8}>
                    <div className="empty-state">No jobs have tested this hypothesis yet.</div>
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
                    style={{ cursor: 'pointer' }}
                    onClick={() => router.push(`/jobs/${job.id}`)}
                    onKeyDown={e => { if (e.key === 'Enter') router.push(`/jobs/${job.id}`) }}
                  >
                    <td className="mono text-dim" style={{ fontSize: 11 }}>{job.id.slice(0, 8)}…</td>
                    <td className="mono">{job.agent_id}</td>
                    <td><Badge status={status}>{status}</Badge></td>
                    <td><TierBadge tier={j.capacity_tier} /></td>
                    <td className="mono" style={{ fontSize: 11 }}>{j.accelerator_count}× {acceleratorLabel(j.accelerator_type)}</td>
                    <td className="mono" style={{ textAlign: 'right', fontSize: 11 }}>
                      {cost != null ? `${cost} AccH` : '—'}
                    </td>
                    <td className="mono" style={{ textAlign: 'right' }}>
                      <JobFinalMetric jobId={job.id} primaryMetric={primaryMetric} />
                    </td>
                    <td className="mono text-dim" style={{ fontSize: 11, whiteSpace: 'nowrap' }}>
                      {relTime(j.submitted_at ?? j.created_at)}
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        </PodContent>
      </Pod>
    </div>
  )
}
