import { createBrowserClient, createServerClient } from '@supabase/ssr'
import type { CookieMethodsServer } from '@supabase/ssr'
import { isAuthRetryableFetchError } from '@supabase/supabase-js'
import type { Provider } from '@supabase/supabase-js'

// Next.js inlines a NEXT_PUBLIC_ variable only where it is named in full, so each is read here
// and nowhere else.
function config(): { url: string; key: string } {
  const url = process.env.NEXT_PUBLIC_SUPABASE_URL
  const key = process.env.NEXT_PUBLIC_SUPABASE_PUBLISHABLE_KEY
  if (!url) throw new Error('NEXT_PUBLIC_SUPABASE_URL is not set: copy .env.example to .env.local and fill it in')
  if (!key) throw new Error('NEXT_PUBLIC_SUPABASE_PUBLISHABLE_KEY is not set: copy .env.example to .env.local and fill it in')
  return { url, key }
}

// The session lives in cookies, which the browser client reads and writes itself and the server
// reads through the request it is handling. Nothing is put in local storage. A UI served over
// https marks them Secure, so the browser never sends the session over plain http.
function browserAuth() {
  const { url, key } = config()
  return createBrowserClient(url, key, { cookieOptions: { secure: window.location.protocol === 'https:' } }).auth
}

/** A request's cookies and whether it reached the UI over https. */
export interface ServerSession {
  cookies: CookieMethodsServer
  https: boolean
}

function serverAuth({ cookies, https }: ServerSession) {
  const { url, key } = config()
  return createServerClient(url, key, { cookies, cookieOptions: { secure: https } }).auth
}

/**
 * Whether the visitor reached the UI over https. A proxy that ends TLS in front of the server
 * says so in X-Forwarded-Proto; without one the request's own scheme is the answer.
 */
export function overHTTPS(headers: Headers, url: URL): boolean {
  const forwarded = headers.get('x-forwarded-proto')
  return (forwarded ? forwarded.split(',')[0].trim() : url.protocol.replace(':', '')) === 'https'
}

export const CALLBACK_PATH = '/auth/callback'
export const LOGIN_PATH = '/login'

/** Every way to sign in. A new one is an entry here and the provider switched on in Supabase. */
export const SIGN_IN_METHODS: { provider: Provider; label: string }[] = [
  { provider: 'github', label: 'Continue with GitHub' },
]

// A stand-in for this app's own origin, to resolve a path against.
const SELF = 'http://self'

/**
 * A place in this app to land on after signing in; anything else is the home page. The value is
 * resolved exactly as a browser would resolve it, tabs, newlines and backslashes included, and
 * kept only when it stays on this origin.
 */
export function safeNext(next: string | null): string {
  if (!next) return '/'
  let url: URL
  try {
    url = new URL(next, SELF)
  } catch {
    return '/'
  }
  return url.origin === SELF ? url.pathname + url.search + url.hash : '/'
}

/** Sends the browser to the provider. It comes back to the callback route, then to `next`. */
export async function signIn(provider: Provider, next: string): Promise<void> {
  const redirectTo = new URL(CALLBACK_PATH, window.location.origin)
  redirectTo.searchParams.set('next', safeNext(next))
  const { error } = await browserAuth().signInWithOAuth({ provider, options: { redirectTo: redirectTo.toString() } })
  if (error) throw error
}

/** Ends this browser's session, leaving the person's other devices signed in. */
export async function signOut(): Promise<void> {
  const { error } = await browserAuth().signOut({ scope: 'local' })
  if (error) throw error
}

/** Why a session stopped: it ran out, or the API would not take its token. */
export type SessionEnd = 'ended' | 'refused'

/**
 * Drops the session and loads the sign-in page, which says why and returns here afterwards. The
 * page is leaving, so the promise never settles and no caller renders a failure on the way out.
 */
export async function endSession(reason: SessionEnd): Promise<never> {
  await signOut()
  const login = new URL(LOGIN_PATH, window.location.origin)
  login.searchParams.set('notice', reason)
  login.searchParams.set('next', window.location.pathname + window.location.search)
  window.location.assign(login)
  return new Promise<never>(() => {})
}

/** The token every API request carries, refreshed when it is about to expire. Null when signed out. */
export async function accessToken(): Promise<string | null> {
  const { data, error } = await browserAuth().getSession()
  if (error) {
    // The identity provider could not be reached: the person is still signed in, the read failed.
    if (isAuthRetryableFetchError(error)) throw error
    // Any other error is the provider refusing to refresh the session, which ends it.
    return null
  }
  return data.session?.access_token ?? null
}

/**
 * Whether the request's cookies hold a session whose token verifies against the project's keys.
 * An expired token is refreshed on the way and the new cookies are written through the session's
 * cookie methods.
 */
export async function hasSession(session: ServerSession): Promise<boolean> {
  const { data, error } = await serverAuth(session).getClaims()
  if (error) {
    // The identity provider could not be reached, which says nothing about the session.
    if (isAuthRetryableFetchError(error)) throw error
    // Any other error is a token that does not verify or a session that cannot be refreshed.
    return false
  }
  return data != null
}

/** Trades the code the provider sent back for a session, written through its cookie methods. */
export async function completeSignIn(session: ServerSession, code: string): Promise<boolean> {
  const { error } = await serverAuth(session).exchangeCodeForSession(code)
  return error == null
}
