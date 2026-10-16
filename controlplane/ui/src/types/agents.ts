import type { Actor } from '@/types/actors'
import type { StatusReason } from '@/types/platform-experiments'

// Mirrors domain.Agent (shared/domain/agent.go).
export interface Agent {
  id: string
  name: string
  top3_count: number       // number of top-3 placements ever (drives +25% bonus)
  created_at: string
}

// What an agent is doing now, in one short sentence, and when it said so.
export interface AgentActivity {
  text: string
  at: string
}

/** A direct message to an agent from its experiment's coordinator or owner, which woke it. */
export interface AgentMessage {
  agent_id: string
  from: 'coordinator' | 'owner'
  text: string
  actor: Actor
  at: string
}

// What became of the machine the platform runs for a hosted agent or a coordinator, as its
// launcher reported it. Mirrors domain.AgentInstance.
export interface AgentInstance {
  // 'waiting' while the agent's platform experiment is open and no machine is asked for,
  // 'requested' until the launcher lists the machine asked for, then as it reports it, and
  // 'stopped' once no further machine is asked for.
  state: 'waiting' | 'requested' | 'starting' | 'running' | 'stopped'
  // When the platform last saw the machine's command make progress. Absent until it first did.
  progress_at?: string
  // When the machine now asked for was asked of the launcher.
  asked_at?: string
  // When the launcher first reported the machine now asked for.
  seen_at?: string
  // Machines that ended and were asked for again, of domain.MaxAgentInstanceRestarts.
  restarts: number
  // The last machine ended with no restart left.
  stopped: boolean
  // Why the last machine that ended was given up on, when, and the end of what it wrote. A
  // machine that was replaced leaves these set while state is running again. 'released' is the
  // platform taking the machine down because its run was over, and exit_log says which end.
  // 'expired' (the provider ended it at its own lifetime) and 'progress_unreadable' (its output
  // could not be read for the progress limit) spend no restart either.
  end_reason?: 'exited' | 'gone' | 'never_seen' | 'still_starting' | 'silent' | 'progress_unreadable' | 'expired' | 'released'
  exited_at?: string
  exit_log?: string
}

// The coordinator the platform runs for a platform experiment. Mirrors domain.HostedCoordinator.
export interface HostedCoordinator {
  launcher: string
  model: string
  instance: AgentInstance
  // Absent on a coordinator that has not reported yet.
  activity?: AgentActivity
}

// One agent's place in a platform experiment and what it was launched with.
export interface AgentSignup {
  platform_experiment_id: string
  agent_id: string
  name: string
  role: 'competitor' | 'baseline' | 'reviewer' | 'planner'
  // Whose credential added the agent.
  actor: Actor
  // true: the platform runs the agent on a machine of its own.
  hosted: boolean
  // The agent type it runs, by id: it runs the type's current version. model is its own, empty
  // when it runs on its type's.
  flavor?: string
  model?: string
  // Whether it is a researcher at work, by the one rule max_agents holds to.
  at_work: boolean
  // Why it no longer holds a place among the researchers, absent while it does.
  ended_reason?: 'finished' | 'cut' | 'replaced' | 'stopped'
  // Why the platform cut it other than at a stage boundary: max_agents_lowered.
  cut_reason?: string
  // The researcher whose place the platform gave this one, and the one given this one's place.
  replaces?: string
  replaced_by?: string
  signed_up_at: string
  // Set once the agent said it has nothing left to do, which is final, with why.
  finished_at?: string
  finished_reason?: string
  // Absent for an agent the platform does not run.
  instance?: AgentInstance
  // Absent on an agent that has not reported yet.
  activity?: AgentActivity
}

// An agent as the roster and the answer to adding it carry it. token is set in the answer to
// adding one the platform does not run. status_reason is set while the agent is not at work yet:
// it waits for its open platform experiment to start, its instance reading waiting and no
// machine asked for, or its machine is asked for and not running (instance_starting).
export interface JoinedAgent extends AgentSignup {
  token?: string
  status_reason?: StatusReason
}

// Whole-set figures for the filters fetchAgentsPage accepts, aggregated server-side.
export interface AgentStats {
  total: number
  top3_eligible: number
}
