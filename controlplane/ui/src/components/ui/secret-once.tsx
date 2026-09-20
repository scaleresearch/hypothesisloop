'use client'

import { useState } from 'react'
import type { ReactNode } from 'react'
import { Button } from '@/components/ui/button'

/**
 * A secret at the one moment it can be read. It lives in the state of whoever rendered this and
 * nowhere else, so dismissing it or leaving the page is the end of it.
 */
export function SecretOnce({ label, value, note, onDismiss }: {
  label: string
  value: string
  note: ReactNode
  onDismiss: () => void
}) {
  const [copied, setCopied] = useState(false)
  const [copyError, setCopyError] = useState('')

  async function copy() {
    try {
      await navigator.clipboard.writeText(value)
      setCopied(true)
      setCopyError('')
    } catch (e) {
      setCopyError(`Copy failed, select the text instead: ${e instanceof Error ? e.message : String(e)}`)
    }
  }

  return (
    <div className="secret-once" role="status">
      <span className="uppercase-label">{label}</span>
      <pre className="mono secret-once-value" data-testid="secret">{value}</pre>
      <p className="text-muted" style={{ fontSize: 12 }}>{note}</p>
      <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
        <Button size="sm" variant="primary" onClick={copy}>{copied ? 'Copied' : 'Copy'}</Button>
        <Button size="sm" onClick={onDismiss}>I have saved it</Button>
        {copyError && <span className="text-error" style={{ fontSize: 12 }}>{copyError}</span>}
      </div>
    </div>
  )
}
