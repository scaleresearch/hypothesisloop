// GitHub returns to one fixed address that names neither the project nor who started, so what
// ties a return to an attempt is a random value kept in this tab's session storage and sent along
// as `state`. A return whose state does not carry it was not started here and is refused.
const KEY = 'hl-repository-attach'
export const REPOSITORY_PATH = '/settings/repository'

interface Attempt {
  nonce: string
  projectID: string
}

/** Where a return from GitHub stands. */
export type AttachStep =
  | { step: 'start' }
  // The App is installed; the person still has to prove on GitHub who they are.
  | { step: 'authorize'; projectID: string; installationID: number }
  // Both are done; what is left is naming the repository.
  | { step: 'attach'; projectID: string; installationID: number; code: string }
  | { step: 'refused'; reason: string }

function attempt(): Attempt | null {
  const stored = window.sessionStorage.getItem(KEY)
  return stored ? JSON.parse(stored) as Attempt : null
}

function installation(value: string | undefined | null): number | null {
  return value && /^[1-9][0-9]*$/.test(value) ? Number(value) : null
}

function withParams(address: string, params: Record<string, string>): string {
  const url = new URL(address)
  for (const [name, value] of Object.entries(params)) url.searchParams.set(name, value)
  return url.toString()
}

/** Starts an attempt for the project and returns where to install the App. */
export function installAddress(installURL: string, projectID: string): string {
  const nonce = crypto.randomUUID()
  window.sessionStorage.setItem(KEY, JSON.stringify({ nonce, projectID } satisfies Attempt))
  return withParams(installURL, { state: nonce })
}

/** Where the person proves who they are, for the attempt in progress. The state carries the installation. */
export function authorizeAddress(authorizeURL: string, installationID: number): string {
  const current = attempt()
  if (!current) throw new Error('no attach is in progress in this tab')
  return withParams(authorizeURL, {
    redirect_uri: new URL(REPOSITORY_PATH, window.location.origin).toString(),
    state: `${current.nonce}.${installationID}`,
  })
}

export function endAttach(): void {
  window.sessionStorage.removeItem(KEY)
}

const NOT_STARTED_HERE = 'This return from GitHub does not belong to an attach started in this tab, so nothing was done with it.'

/** Reads the query GitHub returned with against the attempt this tab started. */
export function attachStep(params: URLSearchParams): AttachStep {
  const state = params.get('state')
  const code = params.get('code')
  const installed = params.get('installation_id')
  const error = params.get('error')
  if (state == null && code == null && installed == null && error == null) return { step: 'start' }

  const current = attempt()
  const [nonce, carried, ...rest] = (state ?? '').split('.')
  if (!current || nonce !== current.nonce || rest.length > 0) return { step: 'refused', reason: NOT_STARTED_HERE }
  if (error != null) {
    return { step: 'refused', reason: `GitHub did not authorise the App: ${params.get('error_description') ?? error}` }
  }
  if (code != null) {
    const installationID = installation(carried)
    if (installationID == null) return { step: 'refused', reason: NOT_STARTED_HERE }
    return { step: 'attach', projectID: current.projectID, installationID, code }
  }
  const installationID = installation(installed)
  if (installationID == null || carried !== undefined) {
    return {
      step: 'refused',
      reason: 'GitHub returned without an installation. When installing needs the approval of an owner of the GitHub organization, start again once it is given.',
    }
  }
  return { step: 'authorize', projectID: current.projectID, installationID }
}

/** Leaves the app for an address on GitHub. */
export function leaveFor(address: string): void {
  window.location.assign(address)
}
