import type { Actor } from '@/types/actors'

export type OrgRole = 'owner' | 'member'

export interface Org {
  id: string
  name: string
  // The caller's own role. Absent for staff looking at an org they do not belong to.
  role?: OrgRole
  created_at: string
}

export interface Project {
  id: string
  org_id: string
  name: string
  created_at: string
}

// Who the session is and what it reaches: see GET /me. Staff reach every org.
export interface Me {
  kind: 'user' | 'staff' | 'key'
  user_id?: string
  name?: string
  orgs: Org[]
  projects: Project[]
}

// One person of an org: the id their identity provider gave them and the name it states, empty
// when it states none. The platform holds no email for them.
export interface OrgMember {
  org_id: string
  user_id: string
  name: string
  role: OrgRole
  joined_at: string
}

// A pending invite to an org. No read of the API returns its token.
export interface OrgInvite {
  id: string
  org_id: string
  role: OrgRole
  /** The person answerable for it: who made it, or the person behind the API key it was made with. */
  actor: Actor
  created_at: string
  expires_at: string
}

// An invite in the answer to creating it, the one place its token appears.
export interface CreatedOrgInvite extends OrgInvite {
  token: string
}

export interface APIKey {
  id: string
  org_id: string
  // Absent on a key that reaches the whole org.
  project_id?: string
  /** The person the key answers to: who created it, or the person behind the key it was created with. */
  actor: Actor
  // The key this one was created with, absent when a person created it.
  minted_by?: string
  name: string
  created_at: string
  revoked_at?: string
}

// An API key in the answer to creating it, the one place its token appears.
export interface CreatedAPIKey extends APIKey {
  token: string
}
