import { PlatformExperimentStatus } from '@/types'
import type { PlatformExperiment, StatusReason } from '@/types'

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

/** The one place that says which phase an experiment is in, from its status and its status_reason. */
export function phaseOf(pe: Stated): Phase {
  switch (pe.status) {
    case PlatformExperimentStatus.DRAFT: {
      const { reason } = reasonOf(pe)
      return reason === 'planning' || reason === 'revising' ? 'planning' : 'proposal'
    }
    case PlatformExperimentStatus.OPEN: return 'baseline'
    case PlatformExperimentStatus.RUNNING: return 'running'
    case PlatformExperimentStatus.CLOSED: return 'closed'
  }
}
