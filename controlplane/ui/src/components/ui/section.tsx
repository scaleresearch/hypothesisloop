import type { HTMLAttributes, ReactNode } from 'react'
import { Card, CardContent } from '@/components/ui/card'
import { Heading } from '@/components/ui/text'

interface SectionProps extends Omit<HTMLAttributes<HTMLDivElement>, 'title'> {
  title: ReactNode
  /** What stands beside the title: a count, a scope. */
  note?: ReactNode
  /** Controls at the right of the heading. */
  actions?: ReactNode
  /** For a table wider than the page. */
  scrollX?: boolean
  children: ReactNode
}

/** A card with a heading: the unit every page is built of. */
export function Section({ title, note, actions, scrollX, children, ...props }: SectionProps) {
  return (
    <Card {...props}>
      <div className="flex items-center gap-2 border-b px-5 py-3.5">
        <Heading level={2} className="min-w-0">
          {title}
          {note !== undefined && note !== null && note !== false && <span className="ml-2 text-xs font-normal text-muted">{note}</span>}
        </Heading>
        {actions && <div className="ml-auto flex flex-none items-center gap-2">{actions}</div>}
      </div>
      <CardContent scrollX={scrollX}>{children}</CardContent>
    </Card>
  )
}
