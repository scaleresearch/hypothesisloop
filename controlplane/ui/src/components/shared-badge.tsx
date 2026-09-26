'use client'

import { isReadOnly } from '@/lib/read-only'
import { useTenant } from '@/lib/tenant'
import { Pill } from '@/components/ui/text'
import type { PlatformExperiment } from '@/types'

/** Marks a platform experiment the platform shares with every account, wherever one is shown. */
export function SharedBadge({ pe }: { pe: Pick<PlatformExperiment, 'shared' | 'project_id'> }) {
  const { me } = useTenant()
  if (!pe.shared) return null
  const readOnly = isReadOnly(me, pe)
  return (
    <Pill
      tone="info"
      title={readOnly
        ? 'The platform shares this platform experiment with every account. You can read everything in it and change nothing.'
        : 'The platform shares this platform experiment with every account, read only. Your organization owns it.'}
    >
      {readOnly ? 'Shared · read only' : 'Shared'}
    </Pill>
  )
}
