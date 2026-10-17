import type { Change } from '@/types/api'
import { experimentTallies } from '@/lib/keys'

/** Which SWR keys a change makes the page read again. */
export type Reads = (key: unknown) => boolean

const named = (...names: string[]): Reads => key => Array.isArray(key) && names.includes(key[0])

// The keys of one record, by its id as the second element: of the names given, or every one.
const of = (id: string, ...names: string[]): Reads =>
  key => Array.isArray(key) && key[1] === id && (names.length === 0 || names.includes(key[0]))

const any = (...reads: Reads[]): Reads => key => reads.some(read => read(key))

const job = (c: Change) => any(
  of(c.subject, 'job'),
  of(c.platform_experiment_id, 'pe-experiments', 'pe-running', 'pe-agent-jobs', 'pe-stats-by-agent', 'pe-stats-by-hypothesis', 'hypothesis-graph'),
  named('jobs', 'job-stats', 'experiment-stats', 'recent-evictions', 'agents-page', 'agent-stats', 'hypothesis'),
)

const agent = (c: Change) => any(
  of(c.platform_experiment_id, 'pe', 'pe-agents', 'pe-stages', 'pe-quotas'),
  named('agents-page', 'agent-stats', 'agent-count'),
)

const hypothesis = (c: Change) => any(
  of(c.subject, 'hypothesis'),
  of(c.platform_experiment_id, 'hypothesis-graph', 'hypothesis-search', 'pe-hypothesis-stats', 'pe-findings'),
)

const experiment = (c: Change) => any(
  of(c.platform_experiment_id),
  named('platform-experiments', 'platform-experiments-running', 'platform-experiment-search'),
)

// The kinds the page follows, and what each makes it read again. environment.status is left out:
// no page shows an environment.
const READS = {
  'experiment.status': job,
  'experiment.blocked': job,
  'job.held': job,
  'job.duplicate': job,
  'job.underperforming': job,
  'quota.changed': (c: Change) => of(c.platform_experiment_id, 'pe-quotas', 'agent-quota'),
  'stage.boundary': agent,
  'agent.cut': agent,
  'agent.finished': agent,
  'agent.replaced': agent,
  'agent.idle': agent,
  'capacity.idle': agent,
  'agent.activity': agent,
  'agent.instructions': (c: Change) => of(c.platform_experiment_id, 'pe-agents'),
  'agent.message': (c: Change) => of(c.platform_experiment_id, 'pe-messages'),
  'hypothesis.new': hypothesis,
  'hypothesis.status': hypothesis,
  'hypothesis.link': (c: Change) => any(hypothesis(c), of(c.detail, 'hypothesis')),
  'finding.new': hypothesis,
  'comment.new': hypothesis,
  'platform_experiment.status': (c: Change) => any(experiment(c), experimentTallies),
  'platform_experiment.description': experiment,
  'platform_experiment.goal': experiment,
  'platform_experiment.proposal': experiment,
  'platform_experiment.plan': experiment,
  'platform_experiment.baseline': experiment,
  'platform_experiment.removed': (c: Change) => any(experiment(c), experimentTallies),
} satisfies Partial<Record<Change['kind'], (c: Change) => Reads>>

/** The kinds a page's stream asks for: the ones it knows what to read again for. */
export const LIVE_KINDS = Object.keys(READS)

/** What changes make the page read again, every key any of them names. */
export function readsOf(changes: Change[]): Reads {
  return any(...changes.map(c => READS[c.kind as keyof typeof READS](c)))
}
