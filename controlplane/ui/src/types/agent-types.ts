import type { Actor } from '@/types/actors'

export type AgentRole = 'researcher' | 'baseline' | 'coordinator' | 'planner'

export interface AgentTypeSettings {
  // Empty is the platform's default.
  model?: string
  max_subagents: number
  effort: 'low' | 'medium' | 'high' | 'xhigh' | 'max'
  hyperparameters?: Record<string, unknown>
}

/** One immutable version of an agent type. */
export interface AgentTypeVersion {
  agent_type: string
  version: number
  prompt: string
  settings: AgentTypeSettings
  digest: string
  // For a built-in version, the platform release that seeded it.
  release?: number
  created_by?: Actor
  created_at: string
}

/** An agent type with its current version. */
export interface AgentType {
  id: string
  // Empty for a built-in type.
  org_id?: string
  name: string
  // One line on what agents of the type do, empty when none was written.
  description: string
  role: AgentRole
  builtin: boolean
  current_version: number
  archived_at?: string
  created_by?: Actor
  created_at: string
  current: AgentTypeVersion
}

/** An agent type after an edit or a restore, and how many running agents were told of its new version. */
export interface EditedAgentType extends AgentType {
  propagation: { version: number; told: number }
}
