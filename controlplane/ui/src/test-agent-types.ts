import type { AgentRole, CurrentAgentType } from '@/types/api'

const at = '2026-01-01T00:00:00Z'

export const RESEARCHER_PROMPT = 'You are a researcher.\n\n{{rules}}\n\n## hl commands\n\n{{hl_commands}}\n\nagent_id: {{agent_id}}\nhyperparameters: {{hyperparameters}}\n'

/** An agent type as the API answers it. */
export function agentType(id: string, name: string, role: AgentRole, more: Partial<CurrentAgentType> = {}): CurrentAgentType {
  const builtin = more.builtin ?? !id.startsWith('at-')
  const version = more.current_version ?? 1
  return {
    id, name, description: '', role, builtin, current_version: version, created_at: at,
    ...(builtin ? {} : { org_id: 'org_a', created_by: { kind: 'user', id: 'u1', name: 'Ada' } }),
    current: {
      agent_type: id, version, prompt: RESEARCHER_PROMPT, digest: `digest-${id}-${version}`, created_at: at,
      settings: { model: '', max_subagents: 4, effort: 'high', hyperparameters: {} },
    },
    ...more,
  }
}

/** The built-in types, and one researcher type of the org's own. */
export const CATALOG: CurrentAgentType[] = [
  agentType('generalist', 'Generalist', 'researcher', { description: 'No fixed axis: whatever moves the metric, settings, structure, inputs or scale.' }),
  agentType('hyperparameter-search', 'Hyperparameter search', 'researcher', { description: 'Holds structure and inputs fixed and tunes the knobs: rates, sizes, thresholds.' }),
  agentType('architecture-search', 'Architecture search', 'researcher', { description: 'Holds settings and inputs fixed and searches the structure of the method.' }),
  agentType('data-search', 'Data search', 'researcher', { description: 'Holds structure and settings fixed and searches what the method is fed.' }),
  agentType('baseline', 'Baseline', 'baseline', { description: 'Builds the harness, measures the control and each reference, and finishes.' }),
  agentType('planner', 'Planner', 'planner', { description: 'Turns a one-line goal into an experiment and proposes it.' }),
  agentType('coordinator', 'Coordinator', 'coordinator', { description: 'Supervises the run, catches mistakes and files the summary.' }),
  agentType('at-lr', 'LR sweeper', 'researcher', { current_version: 3 }),
]
