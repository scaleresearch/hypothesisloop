'use client'

import { useState } from 'react'
import { LANDING_PATH, signOut } from '@/lib/auth'
import { errorMessage } from '@/lib/errors'
import { Button } from '@/components/ui/button'
import { Inline } from '@/components/ui/stack'
import { InlineError } from '@/components/ui/status-message'

/** Ends this browser's session and loads the landing page in full, so nothing the session read stays in memory. */
export function SignOutButton() {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  async function leave() {
    setBusy(true)
    setError('')
    try {
      await signOut()
    } catch (e) {
      setError(errorMessage(e))
      setBusy(false)
      return
    }
    window.location.assign(LANDING_PATH)
  }

  return (
    <Inline>
      <Button onClick={leave} loading={busy}>Sign out</Button>
      {error && <InlineError>Sign-out failed: {error}</InlineError>}
    </Inline>
  )
}
