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
  cluster_withdrawn: 'Its cluster stopped taking this org’s jobs: requeued, no retry spent',
  stage_cut: 'Cut at stage boundary',
  job_too_long: 'Ran past the stage job-length limit',
  // Historical rows from before the stage ladder replaced the two-phase mechanism.
  phase2_hold: 'Held for Phase 2',
  stuck_pending: 'Stuck pending admission',
  scale_up_timeout: 'Autoscaler did not deliver capacity in time',
  code_unreachable: 'Its code could not be fetched (platform fault, no cost)',
  no_offer: 'No machine was on offer to rent (platform fault, no cost)',
  flavor_mismatch: 'Landed on a different accelerator type than reserved',
}

// Human-readable labels for domain.NotAdmittedReason (shared/domain/constants.go): why a QUEUED
// job hasn't started yet. An unmapped code is shown as it is. Use notAdmittedLabel to render.
const NOT_ADMITTED_REASON_LABELS: Record<string, string> = {
  awaiting_admission: 'Waiting for the scheduler’s first look',
  capacity_unavailable: 'No capacity available',
  outranked: 'Outranked by a higher-priority job',
  summary_gate: 'Held until its agent files the summaries it owes for finished jobs',
  stage_cut: 'Cut at stage boundary',
  job_too_long: 'Would run past the stage job-length limit',
  workload_creation_failed: 'Workload creation failed',
  no_multi_node_cluster: 'No connected cluster can run a multi-node job of this shape',
  no_cluster_for_org: 'No cluster takes this org’s jobs: register one, or ask for a shared one',
  clusters_disabled: 'Every cluster this org can use is turned off for its jobs: an owner turns one on in Settings',
  'waiting-for-scale-up': 'Waiting for an autoscaler node already booting on this cluster',
  no_scalable_capacity: 'No autoscaler cluster had room to try — will retry as clusters free up',
  concurrency_cap: 'At its experiment’s max concurrent accelerators',
  exceeds_accelerator_share: 'Requests more cpu/memory/storage than its accelerators’ share of a node',
  exceeds_cluster_capacity: 'Asks for more than any connected cluster has when idle',
}

// notAdmittedLabel is the label of a job's not_admitted_reason, followed by the scheduler's own
// sentence (not_admitted_message), which the platform stores with every reason.
export function notAdmittedLabel(reason: string, message?: string): string {
  const label = NOT_ADMITTED_REASON_LABELS[reason] ?? reason
  return message ? `${label}: ${message}` : label
}

// splitReason cuts a reason the platform wrote as "code: sentence" at its first ": ", as the
// platform does (domain.SplitQueueReason). Grouping or labelling keys on the code alone, or every
// distinct sentence becomes its own category and no label ever matches.
function splitReason(reason?: string | null): [code: string, sentence: string] {
  if (!reason) return ['', '']
  const cut = reason.indexOf(': ')
  return cut === -1 ? [reason, ''] : [reason.slice(0, cut), reason.slice(cut + 2).trim()]
}

export function evictionCode(reason?: string | null): string {
  return splitReason(reason)[0]
}

// saidLines is a runtime's message one line per row: that of a failed job ends with the last
// lines the job said, joined by ' | '.
export function saidLines(message: string): string[] {
  return message.split(' | ')
}

// evictionDetailLines is the explanation one line per row, empty when the reason carries none.
export function evictionDetailLines(reason?: string | null): string[] {
  const [, detail] = splitReason(reason)
  return detail ? saidLines(detail) : []
}

// evictionLabel is what a reader should see: the mapped human label for the code, followed by the
// scheduler's own explanation when there is one.
export function evictionLabel(reason?: string | null): string {
  const [code, detail] = splitReason(reason)
  const label = EVICTION_REASON_LABELS[code] ?? code
  return detail ? `${label} — ${detail}` : label
}

// evictionCodeLabel labels a code that has already been extracted (e.g. a grouping key), where
// the per-job detail is deliberately not shown.
export function evictionCodeLabel(code: string): string {
  return EVICTION_REASON_LABELS[code] ?? code
}

// Fault classes (shared/domain/fault_class.go). The class is the verdict an agent actually needs:
// an agent seeing its failures are 'infrastructure' stops debugging code that was correct, and a
// stage-cut job stops reading as a failed one. Derived server-side from the same by-reason tally,
// never stored twice.
const FAULT_CLASS_LABELS: Record<string, string> = {
  workload: 'Agent',
  infrastructure: 'Infrastructure',
  policy: 'Platform decision',
  unclassified: 'Unclassified',
}

// Order is meaning, not count: the agent's own failures first because that is the only column it
// can act on, then the environment's, then the platform's own decisions — which are not failures.
const FAULT_CLASS_ORDER = ['workload', 'infrastructure', 'policy', 'unclassified']

function faultClassLabel(faultClass: string): string {
  return FAULT_CLASS_LABELS[faultClass] ?? faultClass
}

// faultBreakdown renders a by-class tally as "Agent (3) · Infrastructure (1)", empty when there
// is nothing to report. Classes with no rows are omitted rather than shown as zero.
export function faultBreakdown(byClass: Record<string, number>): string {
  return FAULT_CLASS_ORDER
    .filter(c => (byClass[c] ?? 0) > 0)
    .map(c => `${faultClassLabel(c)} (${byClass[c]})`)
    .join(' · ')
}
