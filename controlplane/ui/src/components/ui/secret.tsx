import type { ReactNode } from 'react'
import { Button } from '@/components/ui/button'
import { InsetCard } from '@/components/ui/card'
import { CodeBlock } from '@/components/ui/code-block'
import { CopyButton } from '@/components/ui/copy-button'
import { Inline } from '@/components/ui/stack'
import { Eyebrow, Text } from '@/components/ui/text'

/**
 * A token on screen, with a copy control. It lives in the state of whoever rendered this and
 * nowhere else, so dismissing it or leaving the page takes it off the screen. `note` says whether
 * it can be had again, and `dismiss` words the button to match.
 */
export function Secret({ label, value, note, dismiss, onDismiss }: {
  label: string
  value: string
  note: ReactNode
  dismiss: string
  onDismiss: () => void
}) {
  return (
    <InsetCard edge="primary" role="status">
      <Eyebrow>{label}</Eyebrow>
      <CodeBlock className="select-all break-all" data-testid="secret">{value}</CodeBlock>
      <Text variant="note">{note}</Text>
      <Inline>
        <CopyButton value={value} variant="primary" />
        <Button type="button" size="sm" onClick={onDismiss}>{dismiss}</Button>
      </Inline>
    </InsetCard>
  )
}
