import { useEffect, useState } from 'react'
import useSWR, { useSWRConfig } from 'swr'
import { beginGitHubInstall, fetchGitHubApp } from '@/lib/api/github'
import { ApiError } from '@/lib/api/request'
import { orgPath } from '@/lib/routes'
import { errorMessage } from '@/lib/errors'
import { keys, githubReads } from '@/lib/keys'
import type { InstallFinished, InstallStart, OrgGitHubInstallation } from '@/types/api'

/**
 * The platform's GitHub App, read under one key by every component that needs it. An error whose
 * reason is GITHUB_NOT_CONFIGURED says the deployment's git host is not GitHub.
 */
export function useGitHubApp(enabled = true) {
  return useSWR(enabled ? keys.githubApp() : null, fetchGitHubApp)
}

/** The id of the GitHub card in the org's settings, for a link that leads to it. */
export const GITHUB_CARD = 'github'

/** The GitHub card in the settings of an org. */
export function githubCardPath(orgID: string): string {
  return `${orgPath(orgID, 'settings')}#${GITHUB_CARD}`
}

/** Where on GitHub the owner of an account reviews the App's installation: permissions, suspension. */
export function installationSettingsURL(installation: Pick<OrgGitHubInstallation, 'installation_id' | 'account_login' | 'account_type'>): string {
  const id = installation.installation_id
  return installation.account_type === 'User'
    ? `https://github.com/settings/installations/${id}`
    : `https://github.com/organizations/${encodeURIComponent(installation.account_login)}/settings/installations/${id}`
}

// A path of this app: one leading slash and no second, and no backslash or control character,
// which a browser reads as a slash or drops and so turns into a second one that leaves the origin.
function inApp(path: unknown): path is string {
  return typeof path === 'string' && /^\/(?!\/)/.test(path) && !/[\\\u0000-\u001f\u007f]/.test(path)
}

// The install tab and the tab it was opened from talk over one channel of this origin. An install
// is told apart by its state; a change made on GitHub without one asks every tab to read again.
const CHANNEL = 'hl-github-install'

type InstallOutcome = { ok: true; installation: OrgGitHubInstallation } | { ok: false; message: string; reason?: string }

// next is the state an install continues under once GitHub's authorization page sent it on to
// GitHub's install page.
type Message = { state: string; outcome: InstallOutcome } | { state: string; next: string } | { changed: true }

function post(message: Message) {
  const channel = new BroadcastChannel(CHANNEL)
  channel.postMessage(message)
  channel.close()
}

function listen(onMessage: (message: Message) => void): () => void {
  const channel = new BroadcastChannel(CHANNEL)
  channel.onmessage = event => onMessage(event.data as Message)
  return () => channel.close()
}

/** The tab GitHub's install page opens in. */
interface InstallTab {
  location: { href: string }
  close(): void
}

const BLOCKED = 'Your browser blocked the GitHub tab. Allow pop-ups for this site, then press the button again.'

/**
 * Opens GitHub's install page of the App in a new tab and answers the state GitHub hands back.
 * The tab is opened before anything is awaited, inside the click, so the browser lets it open;
 * the address it goes to carries a state the API issued for this org, this person and returnTo.
 */
export async function openInstall(orgID: string, returnTo: string, deps: {
  open: () => InstallTab | null
  begin: (orgID: string, returnTo: string) => Promise<InstallStart>
}): Promise<string> {
  const tab = deps.open()
  if (!tab) throw new Error(BLOCKED)
  try {
    const started = await deps.begin(orgID, returnTo)
    tab.location.href = started.install_url
    return started.state
  } catch (error) {
    tab.close()
    throw error
  }
}

/** Where an install started in this tab stands. */
export type Install =
  | { phase: 'idle' }
  | { phase: 'waiting'; state: string }
  | { phase: 'connected'; installation: OrgGitHubInstallation }
  | { phase: 'failed'; message: string; reason?: string }

const IDLE: Install = { phase: 'idle' }

/**
 * Installs the App from this tab without leaving it: `start`, from a click, opens GitHub in a new
 * tab, and the tab GitHub returns to connects the installation and tells this one, which reads
 * the org's GitHub accounts and repositories again. A change made on GitHub alone, as repositories
 * added from its own settings, has every tab read them again.
 */
export function useGitHubInstall(orgID: string, returnTo: string) {
  const [install, setInstall] = useState<Install>(IDLE)
  const { mutate } = useSWRConfig()
  const waiting = install.phase === 'waiting' ? install.state : null

  useEffect(() => listen(message => {
    if ('state' in message && message.state !== waiting) return
    if ('next' in message) {
      setInstall({ phase: 'waiting', state: message.next })
      return
    }
    void mutate(githubReads)
    if ('outcome' in message) {
      setInstall(message.outcome.ok ? { phase: 'connected', installation: message.outcome.installation } : { phase: 'failed', message: message.outcome.message, reason: message.outcome.reason })
    }
  }), [waiting, mutate])

  function start() {
    openInstall(orgID, returnTo, { open: () => window.open('', '_blank'), begin: beginGitHubInstall }).then(
      state => setInstall({ phase: 'waiting', state }),
      error => setInstall({ phase: 'failed', message: errorMessage(error), reason: error instanceof ApiError ? error.reason : undefined }),
    )
  }
  return { install, start, reset: () => setInstall(IDLE) }
}

/** What the install tab shows once it has taken GitHub's return, when it could not close itself. */
export type ReturnView =
  | { view: 'connected'; account: string; returnTo: string | null }
  | { view: 'failed'; message: string }
  // GitHub returned from a change made on GitHub, which no install of this app started.
  | { view: 'changed' }
  // Nothing was there to connect, so the tab went on to GitHub's install page.
  | { view: 'installing' }
  // Nothing GitHub sends: a visit.
  | { view: 'nothing' }

const REQUESTED = 'GitHub asked an owner of that organization to approve the install. Try again once they have.'
const NO_INSTALLATION = 'GitHub returned without an installation, so nothing was connected. Try again.'

type Finish = (state: string, code: string, installationID: number) => Promise<InstallFinished>

/**
 * Takes GitHub's return to the callback URL in the install tab: with the state, the authorization
 * code and the installation GitHub handed back, the installation is connected through `finish`,
 * once, and the outcome is told to the tab the install started in. A return from GitHub's
 * authorization page has no installation: the API connects the ones the person may reach, or
 * answers GitHub's install page, where this tab goes on through `go`. A return with no state,
 * from a change made on GitHub, has every tab read again and connects nothing.
 */
export function takeReturn(query: string, finish: Finish, go: (url: string) => void = url => window.location.assign(url)): Promise<ReturnView> {
  // React runs an effect twice in development, and a state works once: the second run is
  // answered what the first was.
  if (taken?.query !== query) taken = { query, view: take(new URLSearchParams(query), finish, go) }
  return taken.view
}

let taken: { query: string; view: Promise<ReturnView> } | null = null

async function take(params: URLSearchParams, finish: Finish, go: (url: string) => void): Promise<ReturnView> {
  const state = params.get('state')
  const installed = Number(params.get('installation_id'))
  const hasInstallation = Number.isSafeInteger(installed) && installed > 0
  if (!state) {
    if (!hasInstallation) return { view: 'nothing' }
    post({ changed: true })
    return { view: 'changed' }
  }
  const failed = (message: string, reason?: string): ReturnView => {
    post({ state, outcome: { ok: false, message, reason } })
    return { view: 'failed', message }
  }
  const code = params.get('code') ?? ''
  if (!hasInstallation && (params.get('setup_action') === 'request' || !code)) return failed(params.get('setup_action') === 'request' ? REQUESTED : NO_INSTALLATION)
  try {
    const finished = await finish(state, code, hasInstallation ? installed : 0)
    if (finished.install) {
      post({ state, next: finished.install.state })
      go(finished.install.install_url)
      return { view: 'installing' }
    }
    if (!finished.installation) return failed(NO_INSTALLATION)
    post({ state, outcome: { ok: true, installation: finished.installation } })
    return { view: 'connected', account: finished.installation.account_login, returnTo: inApp(finished.return_to) ? finished.return_to : null }
  } catch (error) {
    return failed(errorMessage(error), error instanceof ApiError ? error.reason : undefined)
  }
}
