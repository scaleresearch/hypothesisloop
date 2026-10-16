import { apiCall, apiFetch, apiFetchPage, apiSend, MAX_LIST_PAGE_SIZE, withQuery } from '@/lib/api/request'
import type { Page } from '@/lib/api/request'
import type { GitHubApp, GitHubInstallation, GitHubInstallFinished, GitHubInstallStart, GitHubRepository } from '@/types/github'

/** The reason the API gives on a deployment whose git host is not GitHub. */
export const GITHUB_NOT_CONFIGURED = 'github_not_configured'

// Answered 503 github_not_configured on a deployment whose git host is not GitHub.
export function fetchGitHubApp(): Promise<GitHubApp> {
  return apiFetch<GitHubApp>('/github/app')
}

// Oldest first, as recorded at connect: GitHub is not asked.
export function fetchGitHubInstallations(orgID: string): Promise<Page<GitHubInstallation>> {
  return apiFetchPage<GitHubInstallation>(`/orgs/${orgID}/github-installations?limit=${MAX_LIST_PAGE_SIZE}`)
}

// For an owner: a state good once, for the caller, and GitHub's install page carrying it.
// `returnTo` is the page of this app the install starts from.
export function beginGitHubInstall(orgID: string, returnTo: string): Promise<GitHubInstallStart> {
  return apiSend<GitHubInstallStart>('POST', `/orgs/${orgID}/github-install-states`, { return_to: returnTo })
}

// What GitHub handed back to the setup URL: the state is taken once, whatever the outcome.
export function finishGitHubInstall(state: string, installationID: number): Promise<GitHubInstallFinished> {
  return apiSend<GitHubInstallFinished>('POST', '/github/installations', { state, installation_id: installationID })
}

export async function disconnectGitHubInstallation(orgID: string, installationID: number): Promise<void> {
  await apiCall('DELETE', `/orgs/${orgID}/github-installations/${installationID}`)
}

// Asks GitHub whether the App is still installed there and may create and push. Refused with
// the reason and what to do, as a create on that account would be.
export async function checkGitHubInstallation(orgID: string, installationID: number): Promise<void> {
  await apiCall('GET', `/orgs/${orgID}/github-installations/${installationID}/check`)
}

// For an owner: where a new repository goes when a create names no account.
export async function makeGitHubInstallationDefault(orgID: string, installationID: number): Promise<void> {
  await apiCall('PUT', `/orgs/${orgID}/github-installations/${installationID}/default`)
}

// Read from GitHub on every call, so a caller that searches as the person types waits for a pause.
export function fetchGitHubRepositories(orgID: string, params: { q?: string; limit?: number } = {}): Promise<Page<GitHubRepository>> {
  return apiFetchPage<GitHubRepository>(withQuery(`/orgs/${orgID}/github-repositories`, params))
}
