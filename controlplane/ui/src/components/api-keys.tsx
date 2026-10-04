'use client'

import { useState } from 'react'
import useSWR from 'swr'
import { createAPIKey, fetchAPIKeys, revokeAPIKey } from '@/lib/api'
import type { APIKey, CreatedAPIKey } from '@/types'
import { useTenant } from '@/lib/tenant'
import { StatusLabel } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { ConfirmDialog } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Select } from '@/components/ui/select'
import { Table } from '@/components/ui/table'
import { Secret } from '@/components/ui/secret'
import { Loading, ErrorMessage } from '@/components/ui/status-message'
import { EmptyState } from '@/components/ui/empty-state'
import { Inline, Stack } from '@/components/ui/stack'
import { Code, Text } from '@/components/ui/text'

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
  const [revoking, setRevoking] = useState<APIKey | null>(null)

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
    setRevoking(null)
    setError('')
    try {
      await revokeAPIKey(key.id)
      await mutate()
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    }
  }

  return (
    <Stack>
      <Inline as="form" onSubmit={create}>
        <Input
          fieldSize="sm"
          className="w-56"
          aria-label="Key name"
          placeholder="key name, e.g. laptop"
          value={name}
          onChange={e => setName(e.target.value)}
        />
        <Select
          fieldSize="sm"
          aria-label="Key reach"
          value={scope}
          onChange={setScope}
          options={[
            { value: WHOLE_ORG, label: 'Whole organization' },
            ...projects.map(p => ({ value: p.id, label: `Project ${p.name} only` })),
          ]}
        />
        <Button size="sm" variant="primary" type="submit" loading={busy} disabled={name.trim() === ''}>Create API key</Button>
      </Inline>
      {error && <ErrorMessage>{error}</ErrorMessage>}

      {created && (
        <Secret
          label={`Token of "${created.name}"`}
          value={created.token}
          note={<>
            Set it as <Code>HL_TOKEN</Code> for the <Code>hl</Code> command
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
        <Table>
          <thead>
            <tr><th>Name</th><th>Reaches</th><th>Created</th><th>Status</th><th /></tr>
          </thead>
          <tbody>
            {keys.map(key => (
              <tr key={key.id}>
                <td>
                  <span className="font-medium">{key.name}</span>
                  {key.minted_by && (
                    <Text as="div" variant="hint">minted from {keys.find(k => k.id === key.minted_by)?.name ?? 'a key no longer listed'}</Text>
                  )}
                </td>
                <td className="text-xs">{scopeLabel(key)}</td>
                <td className="text-xs text-muted">{new Date(key.created_at).toLocaleString()}</td>
                <td className="text-xs">
                  {key.revoked_at
                    ? <span className="text-muted">revoked {new Date(key.revoked_at).toLocaleString()}</span>
                    : <StatusLabel tone="success">active</StatusLabel>}
                </td>
                <td className="text-right">
                  {!key.revoked_at && <Button size="sm" variant="danger" onClick={() => setRevoking(key)}>Revoke</Button>}
                </td>
              </tr>
            ))}
          </tbody>
        </Table>
      )}
      {revoking && (
        <ConfirmDialog
          open
          title={`Revoke "${revoking.name}"?`}
          confirmLabel="Revoke"
          onConfirm={() => revoke(revoking)}
          onCancel={() => setRevoking(null)}
        >
          It stops working at once, with every key minted from it. This cannot be undone.
        </ConfirmDialog>
      )}
    </Stack>
  )
}
