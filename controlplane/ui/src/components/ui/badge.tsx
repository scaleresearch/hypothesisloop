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

/**
 * Human vs agent participant badge, driven by domain.AgentKind. Missing/undefined (older API
 * response, or a field this build predates) is treated as "agent" — that has always been every
 * existing row's real kind.
 */
export function AgentKindBadge({ kind, className }: { kind?: string; className?: string }) {
  const isHuman = kind === 'human'
  return <Badge status={isHuman ? 'human' : 'agent'} className={className}>{isHuman ? 'Human' : 'Agent'}</Badge>
}
