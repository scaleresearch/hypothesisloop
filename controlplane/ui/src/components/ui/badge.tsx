import type { ReactNode } from 'react'
import { clsx } from 'clsx'
import { statusTone } from '@/lib/colors'
import type { Tone } from '@/lib/colors'
import { toneText } from '@/components/ui/tone'

const toneClass: Record<Tone, string> = {
  success: 'bg-success/15 text-success',
  warning: 'bg-warning/15 text-warning',
  danger: 'bg-danger/15 text-danger',
  info: 'bg-info/15 text-info',
  accent: 'bg-accent/15 text-accent',
  purple: 'bg-purple/15 text-purple',
  pink: 'bg-pink/15 text-pink',
  neutral: 'bg-neutral/15 text-neutral',
}

interface BadgeProps {
  children?: ReactNode
  /** A job, platform experiment, tier or hypothesis state; it picks the tone. */
  status?: string
  /** For a pill that is no state of anything. */
  tone?: Tone
  className?: string
  title?: string
}

export function Badge({ children, status, tone, className, title }: BadgeProps) {
  return (
    <span
      title={title}
      className={clsx(
        'inline-flex items-center gap-1.5 whitespace-nowrap rounded-full py-0.5 pl-1.5 pr-2 font-mono text-2xs font-bold uppercase tracking-wide ring-1 ring-inset ring-current',
        toneClass[tone ?? statusTone(status)],
        className,
      )}
    >
      <span aria-hidden="true" className="h-1.5 w-1.5 flex-none rounded-full bg-current" />
      {children ?? status}
    </span>
  )
}

/** A dot in a colour of lib/colors, for a legend entry or a status beside its name. */
export function Dot({ color, className }: { color: string; className?: string }) {
  return <span aria-hidden="true" className={clsx('inline-block h-2 w-2 flex-none rounded-full', className)} style={{ background: color }} />
}

/** A state in words with its dot, where a pill would be too heavy: connected, enabled. */
export function StatusLabel({ tone, children }: { tone: Tone; children: ReactNode }) {
  return (
    <span className={clsx('inline-flex items-center gap-1.5 text-xs font-semibold', toneText[tone])}>
      <span aria-hidden="true" className="h-1.5 w-1.5 flex-none rounded-full bg-current" />
      {children}
    </span>
  )
}

export function TierBadge({ tier }: { tier?: string }) {
  if (!tier) return <span className="text-xs text-muted">—</span>
  return <Badge status={tier}>{tier}</Badge>
}

/** Human or agent participant. A row with no kind is an agent: every row was one before kinds existed. */
export function AgentKindBadge({ kind, className }: { kind?: string; className?: string }) {
  const isHuman = kind === 'human'
  return <Badge status={isHuman ? 'human' : 'agent'} className={className}>{isHuman ? 'Human' : 'Agent'}</Badge>
}

/** Marks a completed job whose agent filed no summary of it. */
export function NoSummary({ className }: { className?: string }) {
  return (
    <span title="This job completed and its agent filed no summary of it" className={clsx('whitespace-nowrap text-2xs font-normal text-warning', className)}>
      no summary
    </span>
  )
}
