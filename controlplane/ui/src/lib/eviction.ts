// Human-readable labels for domain.EvictionReason (shared/domain/types.go). Keep in sync with
// the EvictionReason constants defined there — an unmapped reason falls back to its raw code.
export const EVICTION_REASON_LABELS: Record<string, string> = {
  silent: 'No metrics reported (3× interval)',
  overrun: 'Wall-clock overrun (1.5× declared)',
  crash_loop: 'Pod crash loop (>3 restarts)',
  quota_exhaustion: 'Quota exhausted',
  experiment_closed: 'Platform experiment closed',
  agent_removed: 'Agent removed',
  cancelled: 'Cancelled',
  phase2_hold: 'Held for Phase 2',
  metric_decline: 'Metric declined vs. competitors',
  stuck_pending: 'Stuck pending admission',
}
