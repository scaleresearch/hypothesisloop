'use client'

import useSWR from 'swr'
import Link from 'next/link'
import { useRouter } from 'next/navigation'
import { fetchHypothesis } from '@/lib/api'
import { Pod, PodHeader, PodContent } from '@/components/ui/pod'
import { Badge, TierBadge } from '@/components/ui/badge'
import { Loading } from '@/components/ui/status-message'
import { formatT4h } from '@/lib/format'

function relTime(iso: string) {
  const diffMs = Date.now() - new Date(iso).getTime()
  const s = Math.round(diffMs / 1000)
  if (s < 60) return `${s}s ago`
  if (s < 3600) return `${Math.round(s / 60)}m ago`
  if (s < 86400) return `${Math.round(s / 3600)}h ago`
  return new Date(iso).toLocaleDateString()
}

export default function HypothesisDetailPage({ params }: { params: { id: string } }) {
  const { id } = params
  const router = useRouter()

  const { data, error } = useSWR(id, fetchHypothesis, { refreshInterval: 8_000 })

  if (error) return (
    <div>
      <div className="wa-title"><h1>Hypothesis Not Found</h1></div>
      <p className="text-error">Could not load hypothesis {id}</p>
      <Link href="/hypotheses" className="text-link" style={{ fontSize: 13 }}>← Back to hypotheses</Link>
    </div>
  )

  if (!data) return <Loading />

  const jobs = data.jobs ?? []

  return (
    <div>
      <div className="wa-title" style={{ display: 'flex', alignItems: 'flex-end', gap: 16 }}>
        <div style={{ flex: 1 }}>
          <h1 style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
            <span className="mono text-dim" style={{ fontSize: 14 }}>{data.id.slice(0, 8)}…</span>
          </h1>
          <p style={{ fontStyle: 'italic', marginTop: 4 }}>{data.text}</p>
          <p className="text-muted mono" style={{ fontSize: 11, marginTop: 4 }}>
            Registered by {data.agent_id} · {relTime(data.created_at)}
          </p>
        </div>
        <Link href="/hypotheses" className="text-link" style={{ fontSize: 12, marginBottom: 4 }}>← All hypotheses</Link>
      </div>

      <Pod>
        <PodHeader>
          Jobs testing this hypothesis
          {jobs.length > 0 && <span className="text-muted" style={{ fontWeight: 400, marginLeft: 8 }}>({jobs.length})</span>}
        </PodHeader>
        <PodContent scrollX>
          <table className="wa-table">
            <thead>
              <tr>
                <th>Job ID</th>
                <th>Agent</th>
                <th>Status</th>
                <th>Tier</th>
                <th>GPU</th>
                <th style={{ textAlign: 'right' }}>Est. Cost</th>
                <th style={{ textAlign: 'right' }}>Final Metric</th>
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
                const j = job as any
                const status = j.status ?? 'UNKNOWN'
                const cost = j.estimated_cost_t4h != null ? formatT4h(j.estimated_cost_t4h) : null
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
                    <td className="mono" style={{ fontSize: 11 }}>{j.gpu_count}× {j.gpu_type}</td>
                    <td className="mono" style={{ textAlign: 'right', fontSize: 11 }}>
                      {cost != null ? `${cost} T4h` : '—'}
                    </td>
                    <td className="mono" style={{ textAlign: 'right' }}>
                      <span className={j.final_metric_value != null ? 'accent' : 'text-muted'}>
                        {j.final_metric_value != null ? j.final_metric_value.toFixed(4) : '—'}
                      </span>
                    </td>
                    <td className="mono text-dim" style={{ fontSize: 11, whiteSpace: 'nowrap' }}>
                      {relTime(j.created_at)}
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
