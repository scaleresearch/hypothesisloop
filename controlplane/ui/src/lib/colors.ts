/**
 * Single source of truth for semantic + status colors used outside CSS
 * (recharts fills, inline dots, etc). Keep these in sync with the
 * `--status-*` custom properties and `.badge-*` rules in globals.css —
 * nothing here should invent a color CSS doesn't already know about.
 */

export const semantic = {
  success: '#4ade80',
  warning: '#fbbf24',
  danger: '#f2596b',
  info: '#2dd4bf',
  accent: '#a698ff',
  accentStrong: '#7c6cf0',
  purple: '#d093f7',
  pink: '#f472b6',
  neutral: '#b7bcc9',
} as const

export const statusColors: Record<string, string> = {
  RUNNING: semantic.success,
  COMPLETED: semantic.accent,
  QUEUED: semantic.warning,
  SUBMITTED: semantic.neutral,
  ADMITTED: semantic.info,
  FAILED: semantic.danger,
  EVICTED: semantic.pink,
  REJECTED: semantic.danger,
  CANCELLED: semantic.neutral,
}

export function statusColor(s?: string | null): string {
  return statusColors[s ?? ''] ?? semantic.neutral
}

/** Categorical palette for per-agent series in charts (recharts Line/Cell colors). */
export const agentPalette = [
  semantic.accent,
  semantic.warning,
  semantic.success,
  semantic.pink,
  semantic.danger,
  semantic.info,
  '#f57f17',
  semantic.accentStrong,
]
