import { useSyncExternalStore } from 'react'
import type { ProjectRepo } from '@/types'

const KEY = 'hl-repository-attach'
export const REPOSITORY_PATH = '/settings/repository'

// What this tab left for GitHub with. GitHub returns to one fixed address that names neither the
// project nor the repository, so the return is tied to the attempt by the random value sent as
// `state`. An attempt is read once: a second return with the same state finds nothing.
interface Attempt {
  nonce: string
  projectID: string
  repo: string
}

/**
 * The attach this tab is in the middle of. It lives outside the page because the page is mounted
 * again when the URL is pointed back at the attempt's project, and the code GitHub returned works
 * once.
 */
export type Attach =
  | { state: 'idle' }
  | { state: 'attaching'; projectID: string; repo: string }
  | { state: 'attached'; projectID: string; view: ProjectRepo }
  // GitHub or the API said no. `reason` is the API's reason code when it gave one.
  | { state: 'failed'; projectID: string; repo: string; message: string; reason?: string }
  // A return that no attempt of this tab accounts for. Nothing was sent anywhere.
  | { state: 'refused'; message: string }

const IDLE: Attach = { state: 'idle' }
let current: Attach = IDLE
const listeners = new Set<() => void>()

function set(next: Attach) {
  current = next
  listeners.forEach(listener => listener())
}

function subscribe(listener: () => void) {
  listeners.add(listener)
  return () => { listeners.delete(listener) }
}

export function useAttach(): Attach {
  return useSyncExternalStore(subscribe, () => current, () => IDLE)
}

export function resetAttach(): void {
  set(IDLE)
}

// The same shape the API holds a repository name to.
const REPOSITORY_NAME = /^[A-Za-z0-9][A-Za-z0-9-]*\/[A-Za-z0-9._-]+$/

export function validRepository(name: string): boolean {
  const repository = name.slice(name.indexOf('/') + 1)
  return REPOSITORY_NAME.test(name) && repository !== '.' && repository !== '..' && name.length <= 200
}

/** Where GitHub returns the person after they authorise, which the attach names again. */
export function redirectURI(): string {
  return new URL(REPOSITORY_PATH, window.location.origin).toString()
}

/** Records an attempt to attach `repo` to the project and returns where on GitHub it continues. */
export function beginAttach(authorizeURL: string, projectID: string, repo: string): string {
  const attempt: Attempt = { nonce: crypto.randomUUID(), projectID, repo }
  window.sessionStorage.setItem(KEY, JSON.stringify(attempt))
  set(IDLE)
  const url = new URL(authorizeURL)
  url.searchParams.set('redirect_uri', redirectURI())
  url.searchParams.set('state', attempt.nonce)
  return url.toString()
}

function takeAttempt(state: string | null): Attempt | null {
  const stored = window.sessionStorage.getItem(KEY)
  if (!stored) return null
  const attempt = JSON.parse(stored) as Attempt
  if (attempt.nonce !== state) return null
  window.sessionStorage.removeItem(KEY)
  return attempt
}

const NOT_STARTED_HERE = 'This return from GitHub does not belong to an attach started in this tab, so nothing was done with it.'

/** Whether the query is one GitHub returned with from authorising. */
export function isReturn(params: URLSearchParams): boolean {
  return params.has('code') || params.has('state') || params.has('error')
}

/**
 * Takes a return from GitHub. With the state of this tab's attempt and a code, the repository is
 * attached through `attach`, once; with GitHub's refusal, the attempt ends with its words. Any
 * other return is refused and nothing is sent. Returns the project the attempt began in.
 */
export function takeReturn(
  params: URLSearchParams,
  attach: (projectID: string, repo: string, code: string) => Promise<ProjectRepo>,
): string | null {
  // React runs an effect twice in development, and the first run has already taken the attempt.
  if (current.state !== 'idle') return 'projectID' in current ? current.projectID : null
  const attempt = takeAttempt(params.get('state'))
  if (!attempt) {
    set({ state: 'refused', message: NOT_STARTED_HERE })
    return null
  }
  const { projectID, repo } = attempt
  const code = params.get('code')
  if (code == null) {
    const said = params.get('error_description') ?? params.get('error')
    set({ state: 'failed', projectID, repo, message: said ? `GitHub did not authorise the App: ${said}` : 'GitHub returned without a code.' })
    return projectID
  }
  set({ state: 'attaching', projectID, repo })
  attach(projectID, repo, code).then(
    view => set({ state: 'attached', projectID, view }),
    (error: unknown) => set({
      state: 'failed', projectID, repo,
      message: error instanceof Error ? error.message : String(error),
      reason: (error as { reason?: string }).reason,
    }),
  )
  return projectID
}

/** Leaves the app for an address on GitHub, without leaving this page behind for Back to return to. */
export function leaveFor(address: string): void {
  window.location.replace(address)
}
