import { apiCall, apiFetch, apiFetchPage, apiSend, MAX_LIST_PAGE_SIZE } from '@/lib/api/request'
import type { Page } from '@/lib/api/request'
import type { APIKey, CreatedAPIKey, CreatedOrgInvite, Me, Org, OrgInvite, OrgMember, OrgRole, Project } from '@/types/accounts'

export function fetchMe(): Promise<Me> {
  return apiFetch<Me>(`/me`)
}

// The new org starts with the caller as owner and a project named Default project.
export function createOrg(name: string): Promise<Org> {
  return apiSend<Org>('POST', '/orgs', { name })
}

export function renameOrg(orgID: string, name: string): Promise<Org> {
  return apiSend<Org>('PATCH', `/orgs/${orgID}`, { name })
}

// A name another project of the org carries is answered 409.
export function createProject(orgID: string, name: string): Promise<Project> {
  return apiSend<Project>('POST', `/orgs/${orgID}/projects`, { name })
}

export function renameProject(projectID: string, name: string): Promise<Project> {
  return apiSend<Project>('PATCH', `/projects/${projectID}`, { name })
}

// Oldest first. A member is named by user id alone.
export function fetchOrgMembers(orgID: string): Promise<Page<OrgMember>> {
  return apiFetchPage<OrgMember>(`/orgs/${orgID}/members?limit=${MAX_LIST_PAGE_SIZE}`)
}

// Demoting the last owner is answered 409 with reason last_owner.
export function setOrgMemberRole(orgID: string, userID: string, role: OrgRole): Promise<OrgMember> {
  return apiSend<OrgMember>('PATCH', `/orgs/${orgID}/members/${userID}`, { role })
}

// For an owner, and for a member naming themselves. The last owner is answered 409 with reason last_owner.
export async function removeOrgMember(orgID: string, userID: string): Promise<void> {
  await apiCall('DELETE', `/orgs/${orgID}/members/${userID}`)
}

// The invites that can still be accepted, newest first. No token is listed.
export function fetchOrgInvites(orgID: string): Promise<Page<OrgInvite>> {
  return apiFetchPage<OrgInvite>(`/orgs/${orgID}/invites?limit=${MAX_LIST_PAGE_SIZE}`)
}

// The answer is the only place the invite's token ever appears.
export function createOrgInvite(orgID: string, role: OrgRole): Promise<CreatedOrgInvite> {
  return apiSend<CreatedOrgInvite>('POST', `/orgs/${orgID}/invites`, { role })
}

export async function revokeOrgInvite(inviteID: string): Promise<void> {
  await apiCall('DELETE', `/invites/${inviteID}`)
}

// The signed-in person joins the invite's org with the invite's role. A token that cannot be
// accepted is answered 409 with the reason, one the platform does not know 404.
export function acceptOrgInvite(token: string): Promise<OrgMember> {
  return apiSend<OrgMember>('POST', '/invites/accept', { token })
}

// Newest first, revoked ones included. No token is listed.
export function fetchAPIKeys(orgID: string): Promise<APIKey[]> {
  return apiFetch<{ api_keys: APIKey[] }>(`/orgs/${orgID}/api-keys`).then(r => r.api_keys)
}

// The answer is the only place the key's token ever appears. Without a project the key reaches
// the whole org.
export function createAPIKey(orgID: string, name: string, projectID?: string): Promise<CreatedAPIKey> {
  return apiSend<CreatedAPIKey>('POST', `/orgs/${orgID}/api-keys`, { name, project_id: projectID })
}

export function revokeAPIKey(keyID: string): Promise<APIKey> {
  return apiSend<APIKey>('DELETE', `/api-keys/${keyID}`)
}
