const KEY = 'hl-invite'
export const INVITE_PATH = '/invite'

/**
 * The link an invited person opens. The token rides in the fragment, which a browser sends to no
 * server, so it reaches the platform only in the call that accepts it.
 */
export function inviteLink(token: string): string {
  return `${window.location.origin}${INVITE_PATH}#${token}`
}

/**
 * The invite this tab is holding. A token in the address is moved into the tab's storage and taken
 * out of the address, so it survives the trip through sign-in and stays out of the history.
 */
export function heldInvite(): string | null {
  const fragment = window.location.hash.slice(1)
  if (fragment) {
    window.sessionStorage.setItem(KEY, fragment)
    window.history.replaceState(window.history.state, '', window.location.pathname + window.location.search)
  }
  return window.sessionStorage.getItem(KEY)
}

/** Forgets the invite once the API has answered for it: a token is accepted or refused once. */
export function dropInvite(): void {
  window.sessionStorage.removeItem(KEY)
}
