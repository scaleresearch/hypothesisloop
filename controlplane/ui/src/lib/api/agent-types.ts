import { apiFetch, apiSend } from '@/lib/api/request'
import type { AgentRole, AgentType, AgentTypeSettings, AgentTypeVersion, EditedAgentType } from '@/types/agent-types'

// The built-in types and the org's own, archived ones included.
export function fetchAgentTypes(orgID: string): Promise<AgentType[]> {
  return apiFetch<AgentType[]>(`/orgs/${orgID}/agent-types`)
}

export function fetchAgentType(orgID: string, typeID: string): Promise<AgentType> {
  return apiFetch<AgentType>(`/orgs/${orgID}/agent-types/${encodeURIComponent(typeID)}`)
}

// Newest first.
export function fetchAgentTypeVersions(orgID: string, typeID: string): Promise<AgentTypeVersion[]> {
  return apiFetch<AgentTypeVersion[]>(`/orgs/${orgID}/agent-types/${encodeURIComponent(typeID)}/versions`)
}

export type CreateAgentTypeRequest =
  | { from: string; name?: string }
  | { name: string; description?: string; role: AgentRole; prompt: string; settings: AgentTypeSettings }

// For an owner. A copy (from) takes its role, prompt and settings from the type it copies.
export function createAgentType(orgID: string, req: CreateAgentTypeRequest): Promise<AgentType> {
  return apiSend<AgentType>('POST', `/orgs/${orgID}/agent-types`, req)
}

interface EditAgentTypeRequest {
  /** The version the edit was made from: 409 version_conflict when it is no longer current. */
  if_version: number
  name: string
  /** Left out, the description stays as it was. */
  description?: string
  prompt: string
  settings: AgentTypeSettings
}

// Makes the next version current: running agents of the type apply it at their next turn.
export function editAgentType(orgID: string, typeID: string, req: EditAgentTypeRequest): Promise<EditedAgentType> {
  return apiSend<EditedAgentType>('PUT', `/orgs/${orgID}/agent-types/${encodeURIComponent(typeID)}`, req)
}

// Makes a new version holding version's prompt and settings, current as an edit makes it.
export function restoreAgentType(orgID: string, typeID: string, version: number): Promise<EditedAgentType> {
  return apiSend<EditedAgentType>('POST', `/orgs/${orgID}/agent-types/${encodeURIComponent(typeID)}/restore`, { version })
}

// Refused 409 agent_type_in_use while an experiment that is not closed names it.
export function archiveAgentType(orgID: string, typeID: string): Promise<AgentType> {
  return apiSend<AgentType>('POST', `/orgs/${orgID}/agent-types/${encodeURIComponent(typeID)}/archive`)
}
