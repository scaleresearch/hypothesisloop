import { useSyncExternalStore } from 'react'
import { ApiError, INSTALLATION_CHOICE_REQUIRED } from '@/lib/api'
import type { GitHubInstallation, ReachableInstallation } from '@/types'

const KEY = 'hl-github-connect'

/**
 * Where GitHub returns the person: the callback and the setup address registered on the platform's
 * GitHub App. It carries no page of its own, only the return, and hands on to Settings.
 */
export const GITHUB_RETURN_PATH = '/settings/repository'
/** The id of the GitHub card in the org's settings, for a link that leads to it. */
export const GITHUB_CARD = 'github'

// What this tab left for GitHub with. GitHub returns to one fixed address that names neither the
// org nor the project, so the return is tied to the attempt by the random value sent as `state`.
// An attempt is read once: a second return with the same state finds nothing.
interface Attempt {
  nonce: string
  // 'install' left for the App's install page, which returns with the installation it made and no
  // state; 'authorise' left to prove who the person is, which returns with a code and the state.
  step: 'install' | 'authorise'
  orgID: string
  projectID: string
  installationID?: number
}

/**
 * The connect this tab is in the middle of. It lives outside the page because the return address
 * is another page than the card that shows the outcome, and the code GitHub returned works once.
 */
export type Connect =
  | { state: 'idle' }
  | { state: 'connecting'; orgID: string }
  | { state: 'connected'; orgID: string; installation: GitHubInstallation }
  // The person reaches several installations and named none: they are asked which.
  | { state: 'choice'; orgID: string; installations: ReachableInstallation[] }
  // GitHub or the API said no. `reason` is the API's reason code when it gave one.
  | { state: 'failed'; orgID: string; message: string; reason?: string }
  // A return that no attempt of this tab accounts for. Nothing was sent anywhere.
  | { state: 'refused'; message: string }

const IDLE: Connect = { state: 'idle' }
let current: Connect = IDLE
const listeners = new Set<() => void>()

function set(next: Connect) {
  current = next
  listeners.forEach(listener => listener())
}

function subscribe(listener: () => void) {
  listeners.add(listener)
  return () => { listeners.delete(listener) }
}

export function useConnect(): Connect {
  return useSyncExternalStore(subscribe, () => current, () => IDLE)
}

export function resetConnect(): void {
  set(IDLE)
}

/** Where GitHub returns the person after they authorise, which the connect names again. */
export function redirectURI(): string {
  return new URL(GITHUB_RETURN_PATH, window.location.origin).toString()
}

function keep(attempt: Attempt) {
  window.sessionStorage.setItem(KEY, JSON.stringify(attempt))
  set(IDLE)
}

function held(): Attempt | null {
  const stored = window.sessionStorage.getItem(KEY)
  return stored ? JSON.parse(stored) as Attempt : null
}

interface Origin {
  orgID: string
  /** The project the person was in, to return them to. */
  projectID: string
}

/**
 * Records an attempt to connect GitHub to the org and returns where on GitHub it continues: the
 * App's authorisation. `installationID` names the installation to connect, once the person chose.
 */
export function beginConnect(authorizeURL: string, origin: Origin, installationID?: number): string {
  const attempt: Attempt = { nonce: crypto.randomUUID(), step: 'authorise', ...origin, installationID }
  keep(attempt)
  const url = new URL(authorizeURL)
  url.searchParams.set('redirect_uri', redirectURI())
  url.searchParams.set('state', attempt.nonce)
  return url.toString()
}

/** Records that this tab left to install the App, so its return goes on to connect what it installed. */
export function beginInstall(installURL: string, origin: Origin): string {
  keep({ nonce: crypto.randomUUID(), step: 'install', ...origin })
  return installURL
}

const NOT_STARTED_HERE = 'This return from GitHub does not belong to a connect started in this tab, so nothing was done with it.'

/** What the return page does next: show Settings of a project, or go on to GitHub once more. */
export type Next = { to: 'settings'; projectID: string | null } | { to: 'github'; address: string }

interface Calls {
  authorizeURL: () => Promise<string>
  connect: (orgID: string, code: string, installationID?: number) => Promise<GitHubInstallation>
}

/**
 * Takes a return from GitHub. From installing the App, with the installation it made, the person
 * goes on to authorise for that installation. From authorising, with the state of this tab's
 * attempt and a code, the installation is connected through `connect`, once. With GitHub's refusal
 * the attempt ends with its words. Any other return is refused and nothing is sent.
 */
export function takeReturn(query: string, calls: Calls): Promise<Next> {
  // React runs an effect twice in development, and an attempt is taken once: the second run is
  // answered what the first was.
  if (taken?.query !== query) taken = { query, next: take(new URLSearchParams(query), calls) }
  return taken.next
}

let taken: { query: string; next: Promise<Next> } | null = null

async function take(params: URLSearchParams, calls: Calls): Promise<Next> {
  const attempt = held()
  window.sessionStorage.removeItem(KEY)
  const state = params.get('state')
  const installed = Number(params.get('installation_id'))

  if (attempt?.step === 'install' && state == null && Number.isSafeInteger(installed) && installed > 0) {
    const { orgID, projectID } = attempt
    try {
      return { to: 'github', address: beginConnect(await calls.authorizeURL(), { orgID, projectID }, installed) }
    } catch (error) {
      set({ state: 'failed', orgID, message: error instanceof Error ? error.message : String(error) })
      return { to: 'settings', projectID }
    }
  }
  if (attempt?.step !== 'authorise' || attempt.nonce !== state) {
    // A return to this address with nothing GitHub would send is a visit, and says nothing.
    if (params.has('code') || params.has('state') || params.has('error')) set({ state: 'refused', message: NOT_STARTED_HERE })
    return { to: 'settings', projectID: attempt?.projectID ?? null }
  }

  const { orgID, projectID, installationID } = attempt
  const code = params.get('code')
  if (code == null) {
    const said = params.get('error_description') ?? params.get('error')
    set({ state: 'failed', orgID, message: said ? `GitHub did not authorise the App: ${said}` : 'GitHub returned without a code.' })
    return { to: 'settings', projectID }
  }
  set({ state: 'connecting', orgID })
  calls.connect(orgID, code, installationID).then(
    installation => set({ state: 'connected', orgID, installation }),
    (error: unknown) => {
      if (error instanceof ApiError && error.reason === INSTALLATION_CHOICE_REQUIRED && Array.isArray(error.body?.installations)) {
        set({ state: 'choice', orgID, installations: error.body.installations as ReachableInstallation[] })
        return
      }
      set({
        state: 'failed', orgID,
        message: error instanceof Error ? error.message : String(error),
        reason: error instanceof ApiError ? error.reason : undefined,
      })
    },
  )
  return { to: 'settings', projectID }
}

/** Leaves the app for an address on GitHub, without leaving this page behind for Back to return to. */
export function leaveFor(address: string): void {
  window.location.replace(address)
}
