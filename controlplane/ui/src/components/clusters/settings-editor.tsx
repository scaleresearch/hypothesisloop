'use client'

import { useState } from 'react'
import { updateCluster } from '@/lib/api/clusters'
import { Button } from '@/components/ui/button'
import { FormField } from '@/components/ui/form-field'
import { Input } from '@/components/ui/input'
import { Inline, Stack } from '@/components/ui/stack'
import { InlineError } from '@/components/ui/status-message'
import { Text } from '@/components/ui/text'
import type { ClusterSettings, ListedCluster } from '@/types/api'

type Field = keyof ClusterSettings

const FIELDS: { key: Field; label: string; placeholder: string; unit: string; autoscaler: boolean }[] = [
  { key: 'stuck_pending_timeout_seconds', label: 'stuck-pending timeout', placeholder: 'seconds', unit: 's', autoscaler: false },
  { key: 'scale_up_timeout_seconds', label: 'scale-up timeout', placeholder: 'seconds, under 1800', unit: 's', autoscaler: true },
  { key: 'max_speculative_accelerators', label: 'speculative cap', placeholder: 'accelerators', unit: '', autoscaler: true },
]

/**
 * What one cluster uses in place of the scheduler's defaults. The two settings that act on an
 * autoscaler are offered for a cluster that reports one, or that already has one of them set.
 * Saving sends the fields that changed; an emptied field goes back to the default, which the API
 * takes as 0.
 */
export function ClusterSettingsEditor({ orgID, cluster, onSaved }: { orgID: string; cluster: ListedCluster; onSaved: () => Promise<unknown> }) {
  const fields = FIELDS.filter(f => !f.autoscaler || cluster.autoscaler_enabled || cluster[f.key] != null)
  const saved = (key: Field) => (cluster[key] != null ? String(cluster[key]) : '')
  const [draft, setDraft] = useState<Partial<Record<Field, string>>>({})
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState('')
  const overridden = fields.filter(f => cluster[f.key] != null)
  const value = (key: Field) => draft[key] ?? saved(key)

  async function save(e: React.FormEvent) {
    e.preventDefault()
    setError('')
    const change: ClusterSettings = {}
    for (const { key, label } of fields) {
      const text = value(key).trim()
      if (text === saved(key)) continue
      const n = text === '' ? 0 : Number(text)
      if (!Number.isInteger(n) || n < 0 || (text !== '' && n === 0)) {
        setError(`${label} must be a whole number above 0, or empty for the default`)
        return
      }
      change[key] = n
    }
    if (Object.keys(change).length === 0) return
    setSaving(true)
    try {
      await updateCluster(orgID, cluster.name, change)
      await onSaved()
      setDraft({})
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setSaving(false)
    }
  }

  return (
    <Stack as="form" gap="sm" onSubmit={save}>
      {overridden.length > 0 && (
        <Text as="span" variant="code" tone="muted">
          {overridden.map(f => `${f.label}: ${cluster[f.key]}${f.unit}`).join(' · ')}
        </Text>
      )}
      <Inline align="end">
        {fields.map(f => (
          <FormField key={f.key} label={f.label}>
            <Input
              fieldSize="sm"
              className="w-36"
              inputMode="numeric"
              placeholder={f.placeholder}
              value={value(f.key)}
              onChange={e => setDraft({ ...draft, [f.key]: e.target.value })}
            />
          </FormField>
        ))}
        <Button size="sm" variant="primary" type="submit" loading={saving} disabled={fields.every(f => value(f.key).trim() === saved(f.key))}>Save</Button>
      </Inline>
      <Text as="span" variant="hint">Leave a field empty to use the scheduler default.</Text>
      {error && <InlineError>{error}</InlineError>}
    </Stack>
  )
}
