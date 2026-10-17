'use client'

import { Pill } from '@/components/ui/text'
import type { PlatformExperiment } from '@/types/api'

/** Marks a platform experiment the platform shares with every account, wherever one is shown. */
export function SharedBadge({ pe }: { pe: Pick<PlatformExperiment, 'shared' | 'actions'> }) {
  if (!pe.shared) return null
  const readOnly = pe.actions.length === 0
  return (
    <Pill
      tone="neutral"
      title={readOnly
        ? 'The platform shares this experiment with every account.'
        : 'The platform shares this experiment with every account. Your organization owns it.'}
    >
      {readOnly ? 'Shared · read only' : 'Shared'}
    </Pill>
  )
}
