'use client'

import { useState } from 'react'
import { Button } from '@/components/ui/button'
import { InlineError } from '@/components/ui/status-message'
import { errorMessage } from '@/lib/errors'

/** Copies `value` to the clipboard and says so; a refusal is shown beside it. */
export function CopyButton({ value, variant = 'default' }: { value: string; variant?: 'default' | 'primary' }) {
  const [copied, setCopied] = useState(false)
  const [error, setError] = useState('')

  async function copy() {
    try {
      await navigator.clipboard.writeText(value)
      setCopied(true)
      setError('')
    } catch (e) {
      setError(`Copy failed, select the text instead: ${errorMessage(e)}`)
    }
  }

  return (
    <>
      <Button type="button" size="sm" variant={variant} onClick={copy}>{copied ? 'Copied' : 'Copy'}</Button>
      {error && <InlineError>{error}</InlineError>}
    </>
  )
}
