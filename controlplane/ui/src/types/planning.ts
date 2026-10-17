import type { Actor } from '@/types/actors'
import type { AgentActivity } from '@/types/agents'
import type { MetricDefinition, PlatformExperiment } from '@/types/platform-experiments'

/** One researcher of a proposal or of an acceptance. */
export interface ProposedAgent {
  flavor?: string
  model?: string
  // direction is the way to the goal the planner gave this researcher, in a sentence or two.
  hyperparameters?: Record<string, unknown> & { direction?: string }
}

/** A decision the goal left open, and the answer the planner planned on. */
export interface ProposalQuestion {
  question: string
  answer: string
}

/** What a draft's planner proposed, as the API stored it. */
export interface Proposal {
  // The experiment as its file would state it. The description is the brief every agent reads.
  experiment: {
    name: string
    description: string
    metrics: MetricDefinition[]
    // Each left out is the platform's own setting.
    report_interval_seconds?: number
    stages?: PlatformExperiment['stages']
  }
  budget_accelerator_hours: number
  // The run's length, from its start once the baseline is declared.
  duration_hours: number
  // The minutes measuring the baseline needs from the accept.
  baseline_minutes: number
  agents: ProposedAgent[]
  // The type the jobs are sized for: an accelerator, or CPU cores (units.CPU_TYPE) for work that needs none.
  accelerator_type: string
  // The image its jobs name: a job image the platform offers, by name, or the whole address of
  // another. Absent on a proposal stored before proposals named one.
  image?: string
  // What one node of its jobs asks for, when the planner stated it.
  cpu?: string
  memory?: string
  storage?: string
  metric_reason: string
  measurement: string
  // Why the proposed budget, duration and researchers are the least that reach the goal. Absent
  // on a proposal made before the planner was asked for it.
  sizing_reason?: string
  out_of_scope?: string[]
  risks?: string[]
  // A proposal with questions is never accepted by itself: the person confirms or changes each
  // answer, and accepting adds them to the brief.
  questions?: ProposalQuestion[]
}

/** A platform experiment made from a sentence: a draft, or what a draft became. */
export type Draft = PlatformExperiment & {
  // When the platform closes a draft nobody acts on: the first while its planner has answered
  // nothing, the second once it has and the answer waits for a person.
  planning_ends_at?: string
  proposal_expires_at?: string
  // What the draft's planner is doing now, while its machine is up.
  activity?: AgentActivity
  // On a draft whose planner answered: until when its machine is kept, so that a goal changed
  // before then is answered with nothing to boot.
  planner_ends_at?: string
  proposal?: Proposal
  // Set while a proposal stands. Absent while the planner works, on the first goal or a changed
  // one, and on a draft it declined.
  proposed_at?: string
  // When the platform accepts the proposal as proposed, unless the person accepts it sooner,
  // holds it to edit it, changes the goal or closes it. Absent on a draft that waits for the person.
  accepts_at?: string
  // When and why the planner proposed nothing. The reason stays while a changed goal is planned.
  declined_at?: string
  declined_reason?: string
  // Once its proposal was accepted: when, by whom (absent when the platform accepted it by itself
  // at accepts_at) and the answers given to proposal.questions, in their order. The proposal
  // itself stays as it was proposed.
  accepted_at?: string
  accepted_by?: Actor
  accepted_answers?: string[]
}
