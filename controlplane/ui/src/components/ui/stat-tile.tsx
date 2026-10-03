import type { ReactNode } from 'react'
import { clsx } from 'clsx'
import { Link } from '@/components/ui/link'
import { Eyebrow } from '@/components/ui/text'
import { toneText } from '@/components/ui/tone'
import type { Tone } from '@/lib/colors'

interface StatTileProps {
  label: string
  value: ReactNode
  sub?: string
  tone?: Tone
  /** When set, the whole tile becomes a link (e.g. to a pre-filtered job list). */
  href?: string
  /** The linked tile is the filter in force on the page it leads to. */
  active?: boolean
}

const tileClass = 'block min-w-0 overflow-hidden rounded-lg border bg-surface px-4 py-3.5 shadow-sm'
const linkedClass = 'transition-shadow hover:border-border-strong hover:shadow-md'
const activeClass = 'border-primary bg-primary/5 ring-1 ring-inset ring-primary'

/** One figure of a summary row. Only a tile that is a link lifts on hover. */
export function StatTile({ label, value, sub, tone, href, active }: StatTileProps) {
  const content = (
    <>
      <Eyebrow className="mb-1 block">{label}</Eyebrow>
      <div className={clsx('truncate font-mono text-2xl font-medium tabular-nums tracking-tight', tone ? toneText[tone] : 'text-foreground')} title={typeof value === 'string' ? value : undefined}>{value}</div>
      {sub && <div className="mt-0.5 truncate text-2xs text-subtle" title={sub}>{sub}</div>}
    </>
  )
  if (href) return <Link href={href} aria-current={active ? 'true' : undefined} className={clsx(tileClass, linkedClass, active && activeClass)}>{content}</Link>
  return <div className={tileClass}>{content}</div>
}
