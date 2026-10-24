import type { ReactNode } from 'react'
import { Text } from '@/components/ui/text'
import type { HypothesisStatus } from '@/types/api'

/**
 * What a researcher's curation did to a hypothesis: the one it was merged into, which its work
 * goes under, or why it was retired. Nothing for one that is not curated. into renders the id of
 * the hypothesis it was merged into as a link.
 */
export function Curation({ hypothesis: h, into }: {
  hypothesis: { status?: HypothesisStatus; merged_into?: string; status_reason?: string }
  into: (id: string) => ReactNode
}) {
  if (h.status === 'merged' && h.merged_into) {
    return (
      <Text as="div" variant="note" role="note">
        Merged into {into(h.merged_into)}: tested there from now on{h.status_reason ? `, as ${h.status_reason}` : ''}.
      </Text>
    )
  }
  if (h.status === 'retired') {
    return <Text as="div" variant="note" role="note">Retired{h.status_reason ? `: ${h.status_reason}` : ''}. It takes no job and is no verdict.</Text>
  }
  return null
}
