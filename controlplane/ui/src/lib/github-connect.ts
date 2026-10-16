import { useEffect, useState } from 'react'
import useSWR, { useSWRConfig } from 'swr'
import { ApiError, beginGitHubInstall, fetchGitHubApp } from '@/lib/api'
import { orgPath } from '@/lib/routes'
import type { GitHubInstallation, GitHubInstallFinished, GitHubInstallStart } from '@/types'

/**
 * The platform's GitHub App, read under one key by every component that needs it. An error whose
 * reason is GITHUB_NOT_CONFIGURED says the deployment's git host is not GitHub.
 */
export function useGitHubApp(enabled = true) {
  return useSWR(enabled ? 'github-app' : null, fetchGitHubApp, { shouldRetryOnError: false })
}

/** The setup URL registered on the platform's GitHub App: where GitHub returns the install tab. */
export const GITHUB_RETURN_PATH = '/settings/repository'
/** The id of the GitHub card in the org's settings, for a link that leads to it. */
export const GITHUB_CARD = 'github'

/** The GitHub card in the settings of an org. */
export function githubCardPath(orgID: string): string {
  return `${orgPath(orgID, 'settings')}#${GITHUB_CARD}`
}

/** Where on GitHub the owner of an account reviews the App's installation: permissions, suspension. */
export function installationSettingsURL(installation: Pick<GitHubInstallation, 'installation_id' | 'account_login' | 'account_type'>): string {
  const id = installation.installation_id
  return installation.account_type === 'User'
    ? `https://github.com/settings/installations/${id}`
    : `https://github.com/organizations/${encodeURIComponent(installation.account_login)}/settings/installations/${id}`
}

// A path of this app: one leading slash and no second, and no backslash or control character,
// which a browser reads as a slash or drops and so turns into a second one that leaves the origin.
export function inApp(path: unknown): path is string {
  return typeof path === 'string' && /^\/(?!\/)/.test(path) && !/[\\\u0000-\u001f\u007f]/.test(path)
}

// The install tab and the tab it was opened from talk over one channel of this origin. An install
// is told apart by its state; a change made on GitHub without one asks every tab to read again.
const CHANNEL = 'hl-github-install'

export type InstallOutcome = { ok: true; installation: GitHubInstallation } | { ok: false; message: string; reason?: string }

type Message = { state: string; outcome: InstallOutcome } | { changed: true }

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
export interface InstallTab {
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
  begin: (orgID: string, returnTo: string) => Promise<GitHubInstallStart>
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
  | { phase: 'connected'; installation: GitHubInstallation }
  | { phase: 'failed'; message: string; reason?: string }

const IDLE: Install = { phase: 'idle' }
const GITHUB_KEYS = new Set(['github-installations', 'github-repositories', 'repository-check', 'github-installation-check'])

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
    void mutate(key => Array.isArray(key) && GITHUB_KEYS.has(key[0]))
    if ('state' in message) {
      setInstall(message.outcome.ok ? { phase: 'connected', installation: message.outcome.installation } : { phase: 'failed', message: message.outcome.message, reason: message.outcome.reason })
    }
  }), [waiting, mutate])

  function start() {
    openInstall(orgID, returnTo, { open: () => window.open('', '_blank'), begin: beginGitHubInstall }).then(
      state => setInstall({ phase: 'waiting', state }),
      error => setInstall({ phase: 'failed', message: messageOf(error), reason: error instanceof ApiError ? error.reason : undefined }),
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
  // Nothing GitHub sends: a visit.
  | { view: 'nothing' }

const REQUESTED = 'GitHub asked an owner of that organization to approve the install. Press Install GitHub App again once they have.'
const NO_INSTALLATION = 'GitHub returned without an installation, so nothing was connected. Press Install GitHub App again.'

/**
 * Takes GitHub's return to the setup URL in the install tab: with the state and the installation
 * GitHub handed back, the installation is connected through `finish`, once, and the outcome is
 * told to the tab the install started in. A return with no state, from a change made on GitHub,
 * has every tab read again and connects nothing.
 */
export function takeReturn(query: string, finish: (state: string, installationID: number) => Promise<GitHubInstallFinished>): Promise<ReturnView> {
  // React runs an effect twice in development, and a state works once: the second run is
  // answered what the first was.
  if (taken?.query !== query) taken = { query, view: take(new URLSearchParams(query), finish) }
  return taken.view
}

let taken: { query: string; view: Promise<ReturnView> } | null = null

async function take(params: URLSearchParams, finish: (state: string, installationID: number) => Promise<GitHubInstallFinished>): Promise<ReturnView> {
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
  if (!hasInstallation) return failed(params.get('setup_action') === 'request' ? REQUESTED : NO_INSTALLATION)
  try {
    const finished = await finish(state, installed)
    post({ state, outcome: { ok: true, installation: finished.installation } })
    return { view: 'connected', account: finished.installation.account_login, returnTo: inApp(finished.return_to) ? finished.return_to : null }
  } catch (error) {
    return failed(messageOf(error), error instanceof ApiError ? error.reason : undefined)
  }
}

function messageOf(error: unknown): string {
  return error instanceof Error ? error.message : String(error)
}
