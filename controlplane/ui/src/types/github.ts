import type { Actor } from '@/types/actors'

/** The platform's GitHub App. */
export interface GitHubApp {
  slug: string
  install_url: string
  /** Where a person creates a repository on GitHub themselves. Takes `owner` and `visibility`. */
  new_repository_url: string
}

export type GitHubAccountType = 'User' | 'Organization'

/**
 * An installation of the platform's GitHub App that an org connected: a GitHub account, with the
 * repositories its owner chose on GitHub. A platform experiment of the org may name any of them.
 */
export interface GitHubInstallation {
  org_id: string
  installation_id: number
  /** The GitHub user or organisation the App is installed on. */
  account_login: string
  /** Which of the two it is. Absent on one connected before the type was kept, until it is connected again. */
  account_type?: GitHubAccountType
  /** Who connected it: a user, the API key it was connected with, or staff. */
  connected_actor: Actor
  connected_at: string
  /** A new repository goes to this account when a create names none. At most one is. */
  default: boolean
}

/** An install of the App started for an org: GitHub's install page carries the state. */
export interface GitHubInstallStart {
  state: string
  install_url: string
  expires_at: string
}

/** The installation an install connected, and the page of this app it was started from. */
export interface GitHubInstallFinished {
  org_id: string
  return_to: string
  installation: GitHubInstallation
}

/** A repository a platform experiment of the org may name. */
export interface GitHubRepository {
  /** owner/name. */
  repository: string
  installation_id: number
  private: boolean
}
