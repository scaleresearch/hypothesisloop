import type { CSSProperties, ReactNode } from 'react'
import { clsx } from 'clsx'

interface BadgeProps {
  children: ReactNode
  /** Lowercased and used as the `badge-<status>` CSS class, e.g. "RUNNING" -> badge-running */
  status: string
  className?: string
  style?: CSSProperties
}

/** Status pill driven by the `.badge-*` classes in globals.css (job/tier/platform-experiment statuses). */
export function Badge({ children, status, className, style }: BadgeProps) {
  return (
    <span className={clsx('badge', `badge-${status.toLowerCase()}`, className)} style={style}>
      {children ?? status}
    </span>
  )
}

/** Capacity tier badge (guaranteed/burst) — renders a muted em dash when there's no tier yet. */
export function TierBadge({ tier }: { tier?: string }) {
  if (!tier) return <span className="text-muted">—</span>
  return <Badge status={tier}>{tier}</Badge>
}
