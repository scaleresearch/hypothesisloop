import { createExperimentDraft } from '@/lib/api/drafts'
import { ApiError } from '@/lib/api/request'
import type { CreatePlatformExperimentRequest, ExperimentDraft, PlatformExperiment, Proposal, StatusReason } from '@/types/api'

export const MAX_GOAL_LENGTH = 2000

// The reasons of a draft being planned that move on by themselves. Any other holds planning up.
export const PLANNING_MOVES = new Set(['planning', 'revising', 'repository_creating'])

/** Why agent sandboxes are unavailable, when the API refused a create for it, until GET /agentsandbox answers the whole status reason. */
export function agentSandboxRefusal(error: unknown): StatusReason | null {
  if (!(error instanceof ApiError) || error.reason !== AGENTSANDBOX_NOT_CONFIGURED) return null
  return { reason: error.reason, message: error.message, summary: error.message }
}

const AGENTSANDBOX_NOT_CONFIGURED = 'agentsandbox_not_configured'

/** Keeps a goal as a saved draft of the full form, its brief, for a person who sets the experiment up themselves. */
export function formDraftFromGoal(projectID: string, goal: string, repository: string, branch: string): Promise<ExperimentDraft> {
  return createExperimentDraft(projectID, { form: { description: goal, repository, branch }, step: '1', failure: '' })
}

/** The experiment a proposal states, keyed as the create states it. */
export type ProposedExperiment = Required<Pick<CreatePlatformExperimentRequest, 'name' | 'description' | 'metrics'>> &
  Pick<CreatePlatformExperimentRequest, 'report_interval_seconds' | 'stages'>

/** What a draft's planner proposed, stored with the type its jobs are sized for, CPU_TYPE where it named none. */
export type StoredProposal = Omit<Proposal, 'experiment' | 'accelerator_type'> & { experiment: ProposedExperiment; accelerator_type: string }

/** The proposal of pe, which the API answers as the planner's PUT stored it. */
export function proposalOf(pe: Pick<PlatformExperiment, 'proposal'>): StoredProposal | undefined {
  return pe.proposal as StoredProposal | undefined
}
