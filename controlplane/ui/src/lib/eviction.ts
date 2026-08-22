// Human-readable labels for domain.EvictionReason (shared/domain/constants.go). Keep in sync with
// the EvictionReason constants defined there — an unmapped reason falls back to its raw code.
//
// Not exported: a caller holding the raw map reaches past the "code: detail" parsing below and
// renders unmapped strings the moment a reason carries an explanation. Go through evictionLabel
// (to render) or evictionCodeLabel (to label an already-extracted code).
const EVICTION_REASON_LABELS: Record<string, string> = {
  silent: 'Stopped reporting metrics (3× interval)',
  never_reported_metrics: 'Never reported any metric — reporting path broken, not a hung job',
  resource_disbalance: 'CPU/memory out of proportion to its accelerators',
  unschedulable: 'Could not be scheduled (bad image or container config)',
  quota_exhaustion: 'Quota exhausted',
  experiment_closed: 'Platform experiment closed',
  cancelled: 'Cancelled',
  stage_cut: 'Cut at stage boundary',
  job_too_long: 'Ran past the stage job-length limit',
  // Historical rows from before the stage ladder replaced the two-phase mechanism.
  phase2_hold: 'Held for Phase 2',
  stuck_pending: 'Stuck pending admission',
}

// A reason may carry an explanation after its code, in the same "code: detail" shape the scheduler
// writes into not_admitted_reason — the disbalance evictor states what the job requested, what it
// was entitled to and what that stranded. Grouping or labelling must key on the code alone, or
// every distinct explanation becomes its own category and no label ever matches.
export function evictionCode(reason?: string | null): string {
  if (!reason) return ''
  const cut = reason.indexOf(':')
  return cut === -1 ? reason : reason.slice(0, cut)
}

// evictionDetail is the explanation without the code, empty when the reason carries none.
export function evictionDetail(reason?: string | null): string {
  if (!reason) return ''
  const cut = reason.indexOf(':')
  return cut === -1 ? '' : reason.slice(cut + 1).trim()
}

// evictionLabel is what a reader should see: the mapped human label for the code, followed by the
// scheduler's own explanation when there is one.
export function evictionLabel(reason?: string | null): string {
  if (!reason) return ''
  const code = evictionCode(reason)
  const label = EVICTION_REASON_LABELS[code] ?? code
  const detail = evictionDetail(reason)
  return detail ? `${label} — ${detail}` : label
}

// evictionCodeLabel labels a code that has already been extracted (e.g. a grouping key), where
// the per-job detail is deliberately not shown.
export function evictionCodeLabel(code: string): string {
  return EVICTION_REASON_LABELS[code] ?? code
}
