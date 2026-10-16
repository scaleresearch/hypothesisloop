'use client'

import { useState } from 'react'
import type { RefObject } from 'react'
import { flushSync } from 'react-dom'
import { Check } from 'lucide-react'
import { clsx } from 'clsx'
import { BRIEF_POINTS, covers, withPoint } from '@/lib/brief-points'
import type { BriefPoint } from '@/lib/brief-points'
import { Chip } from '@/components/ui/button'
import { Inline } from '@/components/ui/stack'
import { Text } from '@/components/ui/text'

/**
 * The points a checkable brief states, under the box it is written in. A point the text has a
 * line for is ticked; choosing one adds its line, or goes to it, and asks for it with an example.
 */
export function BriefPoints({ text, onChange, box, className }: {
  text: string
  onChange: (text: string) => void
  box: RefObject<HTMLTextAreaElement | null>
  className?: string
}) {
  const [asked, setAsked] = useState<BriefPoint | null>(null)

  function choose(point: BriefPoint) {
    const next = withPoint(text, point)
    // The box holds the new text before it takes the focus, so that keys typed at once land on the line.
    flushSync(() => {
      setAsked(point)
      onChange(next.text)
    })
    box.current?.focus()
    box.current?.setSelectionRange(next.caret, next.caret)
  }

  return (
    <div className={clsx('grid gap-1.5', className)}>
      <Inline gap="xs">
        <Text as="span" variant="hint" className="mr-1">Spell out</Text>
        {BRIEF_POINTS.map(point => {
          const done = covers(text, point)
          return (
            <Chip key={point.label} type="button" active={done} title={point.ask} onClick={() => choose(point)} className="gap-1">
              {done && <Check aria-hidden="true" size={12} />}
              {point.label}
            </Chip>
          )
        })}
      </Inline>
      {asked && <Text variant="hint">{asked.ask} For example: {asked.example}</Text>}
    </div>
  )
}
