'use client'

import { useState } from 'react'
import { closePlatformExperiment } from '@/lib/api/platform-experiments'
import { Button } from '@/components/ui/button'
import { ConfirmDialog } from '@/components/ui/dialog'
import type { PlatformExperiment } from '@/types/api'

/** The owner's control to close an open or running experiment, behind a confirmation that says what closing does. */
export function CloseExperiment({ pe, onClosed }: { pe: PlatformExperiment; onClosed: () => void }) {
  const [asking, setAsking] = useState(false)

  return (
    <>
      <Button size="sm" onClick={() => setAsking(true)}>Close experiment</Button>
      <ConfirmDialog
        open={asking}
        title="Close this experiment?"
        confirmLabel="Close experiment"
        onConfirm={async () => {
          await closePlatformExperiment(pe.id)
          onClosed()
        }}
        onClose={() => setAsking(false)}
      >
        The agents wind down and every running job stops. Nothing new can be submitted, and it cannot be undone.
      </ConfirmDialog>
    </>
  )
}
