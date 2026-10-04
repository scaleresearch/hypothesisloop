import type { ReactNode } from 'react'
import { Stack } from '@/components/ui/stack'
import { Eyebrow } from '@/components/ui/text'

/** A block under a small caption, for reading. A caption over controls is FieldGroup. */
export function Captioned({ caption, className, children }: { caption: ReactNode; className?: string; children: ReactNode }) {
  return (
    <Stack gap="xs" className={className}>
      <Eyebrow>{caption}</Eyebrow>
      <div>{children}</div>
    </Stack>
  )
}
