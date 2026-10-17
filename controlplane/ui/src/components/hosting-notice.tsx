import type { ReactNode } from 'react'
import { Inline } from '@/components/ui/stack'
import { Callout } from '@/components/ui/status-message'
import { Text } from '@/components/ui/text'
import type { StatusReason } from '@/types/api'

/**
 * Why planning cannot go on right now, in the platform's plain sentence, what that means for the
 * person, and the ways on. The operator's detail is there only in answers to the platform's operators.
 */
export function HostingNotice({ reason, title, meaning, actions }: {
  reason: StatusReason
  title: string
  meaning: ReactNode
  actions?: ReactNode
}) {
  return (
    <Callout tone="warning" className="grid justify-items-start gap-2">
      <Text className="font-medium">{title}</Text>
      <Text>{reason.summary}</Text>
      <Text variant="note" tone="default">{meaning}</Text>
      {actions && <Inline gap="sm">{actions}</Inline>}
      {reason.operator_detail && <Text variant="note">Operator: {reason.operator_detail}</Text>}
    </Callout>
  )
}
