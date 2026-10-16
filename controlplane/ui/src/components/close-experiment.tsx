'use client'

import { useState } from 'react'
import { closePlatformExperiment } from '@/lib/api'
import type { PlatformExperiment } from '@/types'
import { Button } from '@/components/ui/button'
import { ConfirmDialog } from '@/components/ui/dialog'
import { InlineError } from '@/components/ui/status-message'

/** The owner's control to close an open or running experiment, behind a confirmation that says what closing does. */
export function CloseExperiment({ pe, onClosed }: { pe: PlatformExperiment; onClosed: () => void }) {
  const [asking, setAsking] = useState(false)
  const [closing, setClosing] = useState(false)
  const [error, setError] = useState<string | null>(null)

  async function close() {
    setClosing(true)
    setError(null)
    try {
      await closePlatformExperiment(pe.id)
      setAsking(false)
      onClosed()
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setClosing(false)
    }
  }

  return (
    <>
      <Button size="sm" onClick={() => setAsking(true)}>Close experiment</Button>
      <ConfirmDialog
        open={asking}
        title="Close this experiment?"
        confirmLabel={closing ? 'Closing' : 'Close experiment'}
        onConfirm={() => { if (!closing) void close() }}
        onCancel={() => { if (!closing) { setAsking(false); setError(null) } }}
      >
        The agents wind down and every running job stops. Nothing new can be submitted, and it cannot be undone.
        {error && <InlineError className="mt-3 block">{error}</InlineError>}
      </ConfirmDialog>
    </>
  )
}
