import type { PlatformExperiment, StatusReason } from '@/types/api'

export type Phase = 'planning' | 'proposal' | 'baseline' | 'running' | 'closed'

/** The course of an experiment from a sentence to its close, in order. */
export const PHASES: { key: Phase; label: string }[] = [
  { key: 'planning', label: 'Planning' },
  { key: 'proposal', label: 'Proposal' },
  { key: 'baseline', label: 'Preparing baseline' },
  { key: 'running', label: 'Running' },
  { key: 'closed', label: 'Closed' },
]

type Stated = Pick<PlatformExperiment, 'id' | 'status' | 'status_reason'>

/** Why one platform experiment, read by itself, is in its status. Every such read carries it. */
export function reasonOf(pe: Stated): StatusReason {
  if (!pe.status_reason) throw new Error(`platform experiment ${pe.id} came without a status_reason`)
  return pe.status_reason
}

/** Whose move a platform experiment that is not closed waits for. Every read of one says. */
export function waitsFor(pe: Stated): NonNullable<StatusReason['waits_for']> {
  const { reason, waits_for: who } = reasonOf(pe)
  if (!who) throw new Error(`platform experiment ${pe.id} is ${pe.status} for ${reason} and came without waits_for`)
  return who
}

/** What the page offers a person whose move it is: the experiment's edit form, or the agents card. */
type OwnerAction = { opens: 'edit' | 'agents'; label: string }

/**
 * For every status reason that can wait for the owner, what the page offers them. The reasons are
 * those of statusWaiters in controlplane/domain/status_reason.go that wait for the owner,
 * or for the coordinator, whose move is the owner's where a person coordinates. phase.test.ts
 * reads that file and fails when the two differ. null is a move with no control in the strip:
 * the proposal and the goal are below it, a baseline is added or declared with the hl
 * command the API's message gives, which names the platform experiment, the edit form has no
 * field for a metric's bound, and the page's Close experiment button is there in every state, but
 * the strip's sentence gives the hl commands that write its summary and close it.
 */
export const OWNER_ACTIONS: Record<string, OwnerAction | null> = {
  proposed: null,
  declined: null,
  baseline_required: null,
  baseline_undeclared: null,
  no_baseline: null,
  baseline_not_completed: null,
  baseline_without_finding: null,
  baseline_missing_metric: null,
  baseline_breaks_constraint: null,
  baseline_not_on_branch: null,
  baseline_weaker_than_own_job: null,
  budget_below_baseline: { opens: 'edit', label: 'Edit budget' },
  budget_spent: { opens: 'edit', label: 'Edit budget' },
  no_executors: { opens: 'agents', label: 'Add an executor' },
  executors_done: null,
}

/**
 * The one place that says which phase an experiment is in, from its status and its status_reason.
 * A draft is at its proposal while one stands or its planner declined, and planning otherwise,
 * also while something the platform runs holds planning up.
 */
export function phaseOf(pe: Stated & { proposed_at?: string }): Phase {
  switch (pe.status) {
    case 'draft':
      return pe.proposed_at || reasonOf(pe).reason === 'declined' ? 'proposal' : 'planning'
    case 'open': return 'baseline'
    case 'running': return 'running'
    case 'closed': return 'closed'
  }
}
