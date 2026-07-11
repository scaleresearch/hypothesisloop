'use client'

import Link from 'next/link'
import { Suspense } from 'react'
import { useRouter, useSearchParams } from 'next/navigation'
import useSWR from 'swr'
import { fetchHypotheses, fetchExperimentsByPlatformExperiment, fetchPlatformExperiment } from '@/lib/api'
import type { Hypothesis, PlatformExperiment } from '@/types'
import { PageHeader } from '@/components/ui/page-header'
import { Pod, PodHeader, PodContent } from '@/components/ui/pod'
import { Button } from '@/components/ui/button'
import { Loading, ErrorMessage } from '@/components/ui/status-message'

function relTime(iso: string) {
  const diffMs = Date.now() - new Date(iso).getTime()
  const s = Math.round(diffMs / 1000)
  if (s < 60) return `${s}s ago`
  if (s < 3600) return `${Math.round(s / 60)}m ago`
  if (s < 86400) return `${Math.round(s / 3600)}h ago`
  return new Date(iso).toLocaleDateString()
}

export default function HypothesesPage() {
  return (
    <Suspense fallback={<Loading />}>
      <HypothesesPageContent />
    </Suspense>
  )
}

function HypothesesPageContent() {
  const router = useRouter()
  const searchParams = useSearchParams()
  const peID = searchParams.get('pe')

  const { data: hypotheses, error, isLoading, mutate } = useSWR<Hypothesis[]>(
    'hypotheses',
    fetchHypotheses,
    { refreshInterval: 15_000 },
  )

  // Hypothesis is a global registry (no platform_experiment_id of its own) — when scoped to a
  // platform experiment, filter down to hypotheses actually referenced by that experiment's jobs.
  const { data: pe } = useSWR<PlatformExperiment>(
    peID ? ['pe', peID] : null,
    () => fetchPlatformExperiment(peID as string),
  )
  const { data: peExperiments } = useSWR(
    peID ? ['pe-experiments', peID] : null,
    () => fetchExperimentsByPlatformExperiment(peID as string),
  )

  const list = (() => {
    const all = hypotheses ?? []
    if (!peID) return all
    const usedIDs = new Set((peExperiments ?? []).map(e => e.hypothesis_id).filter(Boolean))
    return all.filter(h => usedIDs.has(h.id))
  })()

  return (
    <div>
      <PageHeader
        title="Hypotheses"
        description="Registered research claims — every experiment must reference one of these. Registering text equivalent to an existing hypothesis returns the existing row instead of creating a duplicate."
        actions={<Button size="sm" onClick={() => mutate()}>Refresh</Button>}
      />

      {peID && (
        <div className="text-dim" style={{ marginBottom: 12, fontSize: 13 }}>
          Filtered to hypotheses used in <strong>{pe?.name ?? peID}</strong>.{' '}
          <Link href="/hypotheses" className="text-link">Clear filter</Link>
        </div>
      )}

      {isLoading && <Loading />}
      {error && <ErrorMessage>Cannot reach registry service.</ErrorMessage>}

      <Pod>
        <PodHeader>
          Hypothesis Registry
          {list.length > 0 && <span className="text-muted" style={{ fontWeight: 400, marginLeft: 8 }}>({list.length})</span>}
        </PodHeader>
        <PodContent scrollX>
          <table className="wa-table">
            <thead>
              <tr>
                <th>ID</th>
                <th>Text</th>
                <th>Registered by</th>
                <th>Created</th>
              </tr>
            </thead>
            <tbody>
              {list.length === 0 ? (
                <tr>
                  <td colSpan={4}>
                    <div className="empty-state">No hypotheses registered yet.</div>
                  </td>
                </tr>
              ) : list.map(h => (
                <tr
                  key={h.id}
                  tabIndex={0}
                  role="link"
                  style={{ cursor: 'pointer' }}
                  onClick={() => router.push(`/hypotheses/${h.id}`)}
                  onKeyDown={e => { if (e.key === 'Enter') router.push(`/hypotheses/${h.id}`) }}
                >
                  <td className="mono text-dim" style={{ fontSize: 11 }}>{h.id.slice(0, 8)}…</td>
                  <td style={{ maxWidth: 480, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }} title={h.text}>
                    {h.text}
                  </td>
                  <td className="mono">{h.agent_id}</td>
                  <td className="mono text-dim" style={{ fontSize: 11, whiteSpace: 'nowrap' }}>{relTime(h.created_at)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </PodContent>
      </Pod>
    </div>
  )
}
