import type { Actor } from '@/types/actors'
import type { Experiment, JobMetric } from '@/types/jobs'

// The verdict on a claim: see POST /hypotheses/{id}/status. On an agent's hypothesis only that
// agent, or a person or key acting for it, may change it.
export type HypothesisStatus = 'open' | 'confirmed' | 'refuted' | 'inconclusive'

export const HYPOTHESIS_STATUSES: HypothesisStatus[] = ['open', 'confirmed', 'refuted', 'inconclusive']

export interface Hypothesis {
  id: string
  /** The agent whose hypothesis it is, empty on one no agent owns: such a row sits in the same
   *  pool under the same dedup, but owns no job, holds no quota and appears in no standings. */
  agent_id: string
  /** Who registered it, which is another credential than the agent's when one registered it for the agent. */
  actor: Actor
  platform_experiment_id: string
  text: string
  status: HypothesisStatus
  created_at: string
  /** When the verdict was last set; null while it has never been set. */
  status_changed_at: string | null
  /** Who last set the verdict; null while it has never been set. */
  status_actor: Actor | null
}

export type HypothesisLinkType = 'builds_on' | 'contradicts' | 'related'

// A row of GET /hypotheses and a node of the graph. `builds_on` names the outgoing builds_on
// targets; the objective root appears as the platform_experiment_id.
export interface HypothesisListItem extends Hypothesis {
  builds_on: string[]
  child_count: number
  finding_count: number
  comment_count: number
}

// actor and created_at say who made the link and when; agent_id is the agent it was made for.
interface HypothesisGraphEdge {
  from: string
  to: string
  type: HypothesisLinkType
  agent_id: string
  actor: Actor
  created_at: string
}

// What was found for a hypothesis or for the objective: its newest finding, and the job testing it
// that did best on the ranking metric with the summary filed for that job. Each is absent when
// there is none.
export interface Evidence {
  finding?: HypothesisFinding
  best_job?: { id: string; metric: JobMetric; summary?: string }
}

export interface HypothesisGraphNode extends HypothesisListItem, Evidence {}

// The objective's evidence with the comments on it: comment_count of them and the newest five,
// oldest first. Comments on the objective are kept under the platform_experiment_id.
export interface ObjectiveEvidence extends Evidence {
  comments?: HypothesisComment[]
  comment_count?: number
}

// GET /hypotheses/graph: one platform experiment's whole graph. `root` is the objective's id,
// equal to the platform_experiment_id. `objective` is the baseline's evidence: a baseline agent's
// jobs test no hypothesis and hang on the root.
export interface HypothesisGraph {
  root: string
  objective: ObjectiveEvidence
  nodes: HypothesisGraphNode[]
  edges: HypothesisGraphEdge[]
}

// One neighbour on GET /hypotheses/{id}. When it is the objective root, hypothesis_id is the
// platform_experiment_id and text and status are empty. actor and created_at say who made the
// link and when; agent_id is the agent it was made for.
export interface HypothesisLink {
  type: HypothesisLinkType
  hypothesis_id: string
  text: string
  status: HypothesisStatus | ''
  agent_id: string
  actor: Actor
  created_at: string
}

export interface HypothesisLinkInput {
  type: HypothesisLinkType
  to: string
}

// A post-run write-up an agent filed after one of its jobs testing this hypothesis reached
// a terminal state — attached to the hypothesis (not the job), so it joins the shared,
// accumulated evidence trail for that claim. See POST /experiments/{id}/summary.
export interface HypothesisFinding {
  id: string
  hypothesis_id: string
  experiment_id: string
  agent_id: string
  /** Who filed it; agent_id is the job's agent, whose finding it is. */
  actor: Actor
  summary: string
  created_at: string
}

// A freeform, job-independent note against a hypothesis — as opposed to a finding, which
// requires a terminal job behind it.
export interface HypothesisComment {
  id: string
  hypothesis_id: string
  agent_id: string
  actor: Actor
  text: string
  created_at: string
}

// One agent's code for a hypothesis: the branch its jobs ran on and the commit the newest of them
// ran, repository_url@sha.
export interface Implementation {
  agent_id: string
  branch: string
  code_ref: string
}

// GET /hypotheses/{id}: a hypothesis plus the jobs submitted against it, the findings filed
// against it, and its comments. Each list is one bounded page of a set that only grows; the
// matching *_count is the full size, so a short list and a truncated one are never confused.
export interface HypothesisWithJobs extends Hypothesis {
  implementations: Implementation[]
  /** The commit a new implementation starts from, repository_url@sha. Absent while it would start from a baseline nobody declared. */
  base_ref?: string
  jobs: Experiment[]
  job_count: number
  findings: HypothesisFinding[]
  finding_count: number
  comments: HypothesisComment[]
  comment_count: number
  links: { out: HypothesisLink[]; in: HypothesisLink[] }
}

export interface HypothesisStats {
  total: number
  /** A status no hypothesis holds is absent. */
  by_status: Partial<Record<HypothesisStatus, number>>
}

// A hypothesis with a verdict, a finding or a job with a metric.
export interface HypothesisEvidence extends Hypothesis, Evidence {}

export interface HypothesisSearchHit {
  id: string
  text: string
  status: HypothesisStatus
}

export interface RegisteredHypothesis extends Hypothesis {
  already_existed: boolean
  /** Its outgoing links as stored. */
  links: HypothesisLink[]
  /** The hypotheses it reads close to, or the refuted ones it builds on, as a hint to the person. */
  similar: SimilarHypothesis[]
}

export interface SimilarHypothesis {
  id: string
  agent_id?: string
  actor: Actor
  builds_on: string[]
  status: HypothesisStatus
  text: string
  reason: 'close_text' | 'refuted_parent'
}
