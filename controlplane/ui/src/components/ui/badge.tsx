import type { ReactNode } from 'react'
import { clsx } from 'clsx'
import { statusTone } from '@/lib/colors'
import type { Tone } from '@/lib/colors'
import { Dot } from '@/components/ui/dot'
import { toneText } from '@/components/ui/tone'

const toneClass: Record<Tone, string> = {
  success: 'bg-success-tint text-success',
  warning: 'bg-warning-tint text-warning',
  danger: 'bg-danger-tint text-danger',
  info: 'bg-info-tint text-info',
  accent: 'bg-accent-tint text-accent',
  neutral: 'bg-neutral-tint text-neutral',
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
        'inline-flex items-center gap-1.5 whitespace-nowrap rounded-full py-0.5 pl-2 pr-2.5 text-xs font-medium',
        toneClass[tone ?? statusTone(status)],
        className,
      )}
    >
      <Dot size="sm" />
      {/* States come as RUNNING or closed; every badge reads in sentence case. */}
      <span className="inline-block lowercase first-letter:uppercase">{children ?? status}</span>
    </span>
  )
}

/** A state in words with its dot, where a pill would be too heavy: connected, enabled. */
export function StatusLabel({ tone, pulse, title, children, className }: { tone: Tone; pulse?: boolean; title?: string; children: ReactNode; className?: string }) {
  return (
    <span title={title} className={clsx('inline-flex items-center gap-1.5 text-xs font-medium', toneText[tone], className)}>
      <Dot size="sm" pulse={pulse} />
      {children}
    </span>
  )
}

export function TierBadge({ tier }: { tier?: string }) {
  if (!tier) return <span className="text-xs text-muted">—</span>
  return <Badge status={tier}>{tier}</Badge>
}

