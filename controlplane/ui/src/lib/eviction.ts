// Human-readable labels for domain.EvictionReason (shared/domain/types.go). Keep in sync with
// the EvictionReason constants defined there — an unmapped reason falls back to its raw code.
export const EVICTION_REASON_LABELS: Record<string, string> = {
  silent: 'No metrics reported (3× interval)',
  crash_loop: 'Pod crash loop (>3 restarts)',
  quota_exhaustion: 'Quota exhausted',
  experiment_closed: 'Platform experiment closed',
  agent_removed: 'Agent removed',
  cancelled: 'Cancelled',
  stage_cut: 'Cut at stage boundary',
  // Historical rows from before the stage ladder replaced the two-phase mechanism.
  phase2_hold: 'Held for Phase 2',
  stuck_pending: 'Stuck pending admission',
}
