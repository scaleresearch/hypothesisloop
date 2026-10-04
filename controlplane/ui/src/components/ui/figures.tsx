import type { ReactNode } from 'react'
import { clsx } from 'clsx'
import type { Tone } from '@/lib/colors'
import { TextLink } from '@/components/ui/link'
import { toneText } from '@/components/ui/tone'

export interface Figure {
  label: string
  value: ReactNode
  /** What the figure is made of, on hover. */
  title?: string
  tone?: Tone
  /** Where the records behind the figure are listed. */
  href?: string
}

/** A quiet row of a few figures in place of a wall of tiles. */
export function Figures({ label, figures }: { label: string; figures: Figure[] }) {
  if (figures.length === 0) return null
  return (
    <ul aria-label={label} className="flex flex-wrap items-baseline gap-x-6 gap-y-1 text-sm">
      {figures.map(f => {
        const content = (
          <>
            <span className="text-muted">{f.label}</span>{' '}
            <span className={clsx('font-medium tabular-nums', f.tone ? toneText[f.tone] : 'text-foreground')}>{f.value}</span>
          </>
        )
        return (
          <li key={f.label} title={f.title}>
            {f.href ? <TextLink href={f.href}>{content}</TextLink> : content}
          </li>
        )
      })}
    </ul>
  )
}
