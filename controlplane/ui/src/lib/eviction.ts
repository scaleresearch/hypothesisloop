// Human-readable labels for domain.EvictionReason (shared/domain/types.go). Keep in sync with
// the EvictionReason constants defined there — an unmapped reason falls back to its raw code.
export const EVICTION_REASON_LABELS: Record<string, string> = {
  silent: 'Stopped reporting metrics (3× interval)',
  never_reported_metrics: 'Never reported any metric — reporting path broken, not a hung job',
  resource_disbalance: 'CPU/memory out of proportion to its accelerators',
  unschedulable: 'Could not be scheduled (bad image or container config)',
  crash_loop: 'Pod crash loop (>3 restarts)',
  quota_exhaustion: 'Quota exhausted',
  experiment_closed: 'Platform experiment closed',
  agent_removed: 'Agent removed',
  cancelled: 'Cancelled',
  stage_cut: 'Cut at stage boundary',
  job_too_long: 'Ran past the stage job-length limit',
  // Historical rows from before the stage ladder replaced the two-phase mechanism.
  phase2_hold: 'Held for Phase 2',
  stuck_pending: 'Stuck pending admission',
}
