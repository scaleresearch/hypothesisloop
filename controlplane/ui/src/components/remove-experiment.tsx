'use client'

import { useState } from 'react'
import { useSWRConfig } from 'swr'
import { removePlatformExperiment } from '@/lib/api/platform-experiments'
import type { PlatformExperiment } from '@/types/platform-experiments'
import { Button } from '@/components/ui/button'
import { ConfirmDialog } from '@/components/ui/dialog'

// The reads a removed experiment no longer counts in.
const LISTS = new Set(['platform-experiments', 'experiment-stats', 'all-experiment-stats', 'agent-count'])

/** The owner's control to remove an experiment, behind a confirmation that says what removing does. */
export function RemoveExperiment({ pe, label = 'Remove experiment', onRemoved }: { pe: PlatformExperiment; label?: string; onRemoved?: () => void }) {
  const { mutate } = useSWRConfig()
  const [asking, setAsking] = useState(false)

  return (
    <>
      <Button size="sm" onClick={() => setAsking(true)}>{label}</Button>
      <ConfirmDialog
        open={asking}
        title="Remove this experiment?"
        confirmLabel="Remove experiment"
        onConfirm={async () => {
          await removePlatformExperiment(pe.id)
          void mutate(key => Array.isArray(key) && LISTS.has(key[0]))
          onRemoved?.()
        }}
        onClose={() => setAsking(false)}
      >
        The experiment disappears from the platform. Its running agents and jobs stop, and its results are kept.
      </ConfirmDialog>
    </>
  )
}
