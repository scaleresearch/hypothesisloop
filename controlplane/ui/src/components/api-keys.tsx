'use client'

import { useState } from 'react'
import useSWR from 'swr'
import { createAPIKey, fetchAPIKeys, revokeAPIKey } from '@/lib/api'
import type { APIKey, CreatedAPIKey } from '@/types'
import { useTenant } from '@/lib/tenant'
import { Button } from '@/components/ui/button'
import { Secret } from '@/components/ui/secret'
import { Loading, ErrorMessage, EmptyState } from '@/components/ui/status-message'

const WHOLE_ORG = ''

/** The org's API keys: create one and read its token once, list them, revoke one. */
export function APIKeys() {
  const { me, org } = useTenant()
  const { data: keys, error: listError, mutate } = useSWR(['api-keys', org.id], () => fetchAPIKeys(org.id))
  const [name, setName] = useState('')
  const [scope, setScope] = useState(WHOLE_ORG)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  // The token exists here and nowhere else: no read of the API returns it again.
  const [created, setCreated] = useState<CreatedAPIKey | null>(null)

  const projects = me.projects.filter(p => p.org_id === org.id)
  const scopeLabel = (key: APIKey) =>
    key.project_id ? `project ${projects.find(p => p.id === key.project_id)?.name ?? key.project_id}` : 'whole organization'

  async function create(e: React.FormEvent) {
    e.preventDefault()
    setBusy(true)
    setError('')
    try {
      setCreated(await createAPIKey(org.id, name.trim(), scope === WHOLE_ORG ? undefined : scope))
      setName('')
      await mutate()
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  async function revoke(key: APIKey) {
    if (!confirm(`Revoke "${key.name}"? It stops working at once, with every key minted from it. This cannot be undone.`)) return
    setError('')
    try {
      await revokeAPIKey(key.id)
      await mutate()
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    }
  }

  return (
    <div style={{ display: 'grid', gap: 12 }}>
      <form className="settings-row" onSubmit={create}>
        <input
          className="field-sm"
          style={{ width: 220 }}
          aria-label="Key name"
          placeholder="key name, e.g. laptop"
          value={name}
          onChange={e => setName(e.target.value)}
        />
        <select className="field-sm" aria-label="Key reach" value={scope} onChange={e => setScope(e.target.value)}>
          <option value={WHOLE_ORG}>Whole organization</option>
          {projects.map(p => <option key={p.id} value={p.id}>Project {p.name} only</option>)}
        </select>
        <Button size="sm" variant="primary" type="submit" disabled={busy || name.trim() === ''}>
          {busy ? 'Creating…' : 'Create API key'}
        </Button>
      </form>
      {error && <ErrorMessage>{error}</ErrorMessage>}

      {created && (
        <Secret
          label={`Token of "${created.name}"`}
          value={created.token}
          note={<>
            Set it as <span className="mono">HL_TOKEN</span> for the <span className="mono">hl</span> command
            line or an outside agent. This is the only time it is shown: the platform keeps its
            hash. If you lose it, revoke the key and create another.
          </>}
          dismiss="I have saved it"
          onDismiss={() => setCreated(null)}
        />
      )}

      {listError && <ErrorMessage>Could not load API keys: {listError.message}</ErrorMessage>}
      {!keys && !listError && <Loading />}
      {keys && keys.length === 0 && <EmptyState>No API keys yet.</EmptyState>}
      {keys && keys.length > 0 && (
        <table className="wa-table">
          <thead>
            <tr><th>Name</th><th>Id</th><th>Reaches</th><th>Created</th><th>Status</th><th /></tr>
          </thead>
          <tbody>
            {keys.map(key => (
              <tr key={key.id}>
                <td style={{ fontWeight: 600 }}>{key.name}</td>
                <td className="mono text-muted" style={{ fontSize: 12 }}>
                  {key.id}
                  {key.minted_by && <div style={{ fontSize: 11 }}>minted from {key.minted_by}</div>}
                </td>
                <td style={{ fontSize: 12 }}>{scopeLabel(key)}</td>
                <td className="mono text-muted" style={{ fontSize: 12 }}>{new Date(key.created_at).toLocaleString()}</td>
                <td style={{ fontSize: 12 }}>
                  {key.revoked_at ? `revoked ${new Date(key.revoked_at).toLocaleString()}` : 'active'}
                </td>
                <td>
                  {!key.revoked_at && <Button size="sm" variant="danger" onClick={() => revoke(key)}>Revoke</Button>}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  )
}
