import type { AgentRole, AgentType } from '@/types'

const at = '2026-01-01T00:00:00Z'

export const RESEARCHER_PROMPT = 'You are a researcher.\n\n{{rules}}\n\n## hl commands\n\n{{hl_commands}}\n\nagent_id: {{agent_id}}\nhyperparameters: {{hyperparameters}}\n'

/** An agent type as the API answers it. */
export function agentType(id: string, name: string, role: AgentRole, more: Partial<AgentType> = {}): AgentType {
  const builtin = more.builtin ?? !id.startsWith('at-')
  const version = more.current_version ?? 1
  return {
    id, name, role, builtin, current_version: version, created_at: at, live_agents: 0,
    ...(builtin ? {} : { org_id: 'org_a', created_by: { kind: 'user', id: 'u1', name: 'Ada' } }),
    current: {
      agent_type: id, version, prompt: RESEARCHER_PROMPT, digest: `digest-${id}-${version}`, created_at: at,
      settings: { model: '', max_subagents: 4, pacing_subagents: 2, effort: 'high', hyperparameters: {} },
    },
    ...more,
  }
}

/** The built-in types, and one researcher type of the org's own. */
export const CATALOG: AgentType[] = [
  agentType('generalist', 'Generalist', 'researcher'),
  agentType('hyperparameter-search', 'Hyperparameter search', 'researcher'),
  agentType('architecture-search', 'Architecture search', 'researcher'),
  agentType('data-search', 'Data search', 'researcher'),
  agentType('baseline', 'Baseline', 'baseline'),
  agentType('planner', 'Planner', 'planner'),
  agentType('coordinator', 'Coordinator', 'coordinator'),
  agentType('at-lr', 'LR sweeper', 'researcher', { current_version: 3, live_agents: 2 }),
]
