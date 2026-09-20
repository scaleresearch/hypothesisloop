'use client'

import { useState } from 'react'
import useSWR from 'swr'
import { fetchClusterSettings, putClusterSettings } from '@/lib/api'
import type { ClusterSettings } from '@/types'
import { Button } from '@/components/ui/button'
import { Loading } from '@/components/ui/status-message'

type Field = Exclude<keyof ClusterSettings, 'cluster_id'>

const FIELDS: { key: Field; label: string; placeholder: string; unit: string }[] = [
  { key: 'scale_up_timeout_seconds', label: 'scale-up timeout', placeholder: 'seconds, under 1800', unit: 's' },
  { key: 'max_speculative_accelerators', label: 'speculative cap', placeholder: 'accelerators', unit: '' },
  { key: 'stuck_pending_timeout_seconds', label: 'stuck-pending timeout', placeholder: 'seconds', unit: 's' },
]

const EMPTY: Record<Field, string> = {
  scale_up_timeout_seconds: '', max_speculative_accelerators: '', stuck_pending_timeout_seconds: '',
}

// An empty field is sent as null, which puts the cluster back on the scheduler's own default.
function parse(draft: Record<Field, string>): Required<Omit<ClusterSettings, 'cluster_id'>> {
  const out = { scale_up_timeout_seconds: null, max_speculative_accelerators: null, stuck_pending_timeout_seconds: null } as Record<Field, number | null>
  for (const { key, label } of FIELDS) {
    const text = draft[key].trim()
    if (text === '') continue
    const n = Number(text)
    if (!Number.isInteger(n) || n < 0) throw new Error(`${label} must be a whole number, or empty for the default`)
    out[key] = n
  }
  return out
}

/** One cluster's overrides of the scheduler defaults: see PUT /clusters/{cluster_id}/settings. */
export function ClusterSettingsEditor({ clusterID }: { clusterID: string }) {
  const { data, error: loadError, isLoading, mutate } = useSWR(['cluster-settings', clusterID], () => fetchClusterSettings(clusterID))
  const [draft, setDraft] = useState<Record<Field, string> | null>(null)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState('')

  if (isLoading) return <Loading rows={1} />
  if (loadError || !data) return <span className="text-error" style={{ fontSize: 12 }}>Could not load settings: {loadError?.message}</span>

  if (!draft) {
    return (
      <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
        <span className="mono text-muted" style={{ fontSize: 12 }}>
          {FIELDS.map(f => `${f.label}: ${data[f.key] != null ? `${data[f.key]}${f.unit}` : 'default'}`).join(' · ')}
        </span>
        <Button size="sm" onClick={() => {
          setError('')
          setDraft({ ...EMPTY, ...Object.fromEntries(FIELDS.filter(f => data[f.key] != null).map(f => [f.key, String(data[f.key])])) })
        }}>Edit</Button>
      </div>
    )
  }

  async function save(current: Record<Field, string>) {
    setSaving(true)
    setError('')
    try {
      await putClusterSettings(clusterID, parse(current))
      await mutate()
      setDraft(null)
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setSaving(false)
    }
  }

  return (
    <div style={{ display: 'grid', gap: 6 }}>
      <div style={{ display: 'flex', alignItems: 'flex-end', gap: 8, flexWrap: 'wrap' }}>
        {FIELDS.map(f => (
          <label key={f.key} style={{ display: 'grid', gap: 2 }}>
            <span className="uppercase-label">{f.label}</span>
            <input
              className="field-sm"
              style={{ width: 150 }}
              inputMode="numeric"
              placeholder={f.placeholder}
              value={draft[f.key]}
              onChange={e => setDraft({ ...draft, [f.key]: e.target.value })}
            />
          </label>
        ))}
        <Button size="sm" variant="primary" disabled={saving} onClick={() => save(draft)}>{saving ? 'Saving…' : 'Save'}</Button>
        <Button size="sm" disabled={saving} onClick={() => setDraft(null)}>Cancel</Button>
      </div>
      <span className="text-dim" style={{ fontSize: 11 }}>Leave a field empty to use the scheduler default.</span>
      {error && <span className="text-error" style={{ fontSize: 12 }}>{error}</span>}
    </div>
  )
}
