import { createBrowserClient, createServerClient } from '@supabase/ssr'
import type { CookieMethodsServer } from '@supabase/ssr'
import { isAuthRetryableFetchError } from '@supabase/supabase-js'
import { publicConfig } from '@/lib/config'

// The sign-in service is a Supabase project: IDENTITY_PROVIDER names it, so a
// deployment states which service it signs people in with.
function config(): { url: string; key: string } {
  const provider = publicConfig.identityProvider()
  if (provider !== 'supabase') {
    throw new Error(`IDENTITY_PROVIDER is ${provider ? `'${provider}'` : 'not set'}, and the one sign-in service this UI knows is supabase, as .env.example shows`)
  }
  const url = publicConfig.identitySupabaseURL()
  const key = publicConfig.identitySupabasePublishableKey()
  if (!url) throw new Error('IDENTITY_SUPABASE_URL is not set, as .env.example shows')
  if (!key) throw new Error('IDENTITY_SUPABASE_PUBLISHABLE_KEY is not set, as .env.example shows')
  return { url, key }
}

/** Where the sign-in service is reached, which the browser calls for sessions and tokens. */
export function identityServiceURL(): string {
  return config().url
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

const CALLBACK_PATH = '/auth/callback'
export const LOGIN_PATH = '/login'
/** The public landing page. */
export const LANDING_PATH = '/'
/** The request header the middleware answers whether the request carries a verified session in. */
export const SIGNED_IN_HEADER = 'x-hypothesisloop-signed-in'
/** The signed-in home, where sign-in lands without a way back. */
export const HOME_PATH = '/console'

/** The sign-in page worded for a first visit. Signing up and signing in are the same flow. */
export const SIGN_UP_PATH = `${LOGIN_PATH}?mode=sign-up`

/** Every provider the UI can offer, by the name the identity provider knows it by. */
export const PROVIDER_NAMES = { github: 'GitHub', google: 'Google', apple: 'Apple' } as const
export type SignInProvider = keyof typeof PROVIDER_NAMES

/**
 * The providers this deployment offers, in the order IDENTITY_SIGN_IN_PROVIDERS lists them.
 * A deployment names them itself, so a missing or misspelt list is an error and never a default.
 */
export function signInProviders(): SignInProvider[] {
  const listed = (publicConfig.signInProviders() ?? '').split(',').map(name => name.trim().toLowerCase()).filter(Boolean)
  const known = Object.keys(PROVIDER_NAMES).join(', ')
  if (listed.length === 0) {
    throw new Error(`IDENTITY_SIGN_IN_PROVIDERS is not set: list the sign-in providers to offer (${known}), as .env.example shows`)
  }
  const unknown = listed.filter(name => !(name in PROVIDER_NAMES))
  if (unknown.length > 0) throw new Error(`IDENTITY_SIGN_IN_PROVIDERS names ${unknown.join(', ')}, and the providers are ${known}`)
  return listed.filter((name, i) => listed.indexOf(name) === i) as SignInProvider[]
}

/** The identity provider would not start sign-in with this provider: it is not switched on there. */
export class ProviderRefused extends Error {
  constructor(readonly provider: SignInProvider, readonly detail: string) {
    super(`Sign-in with ${PROVIDER_NAMES[provider]} is not available yet. Choose another way to sign in.`)
    this.name = 'ProviderRefused'
  }
}

// A stand-in for this app's own origin, to resolve a path against.
const SELF = 'http://self'

/**
 * A place in this app to land on after signing in; anything else is the signed-in home. The value is
 * resolved exactly as a browser would resolve it, tabs, newlines and backslashes included, and
 * kept only when it stays on this origin.
 */
export function safeNext(next: string | null): string {
  if (!next) return HOME_PATH
  let url: URL
  try {
    url = new URL(next, SELF)
  } catch {
    return HOME_PATH
  }
  return url.origin === SELF ? url.pathname + url.search + url.hash : HOME_PATH
}

/**
 * Asks the identity provider's authorize address without following it. A provider that is switched
 * on answers with a redirect to itself; one that is not answers 400 with a JSON body, which would
 * otherwise be the page the visitor is left on.
 */
async function refusalOf(authorizeURL: string): Promise<string | null> {
  const answer = await fetch(authorizeURL, { redirect: 'manual' })
  if (answer.type === 'opaqueredirect' || answer.ok) return null
  const body = await answer.json().catch(() => null) as { msg?: string; message?: string } | null
  return body?.msg ?? body?.message ?? `the identity provider answered ${answer.status}`
}

/**
 * Sends the browser to the provider. It comes back to the callback route, then to `next`. A
 * provider the identity provider refuses is a ProviderRefused and the browser stays where it is.
 */
export async function signIn(provider: SignInProvider, next: string): Promise<void> {
  const redirectTo = new URL(CALLBACK_PATH, window.location.origin)
  redirectTo.searchParams.set('next', safeNext(next))
  const { data, error } = await browserAuth().signInWithOAuth({
    provider,
    options: { redirectTo: redirectTo.toString(), skipBrowserRedirect: true },
  })
  if (error) throw error
  if (!data.url) throw new Error('the identity provider gave no address to sign in at')
  const refusal = await refusalOf(data.url)
  if (refusal) throw new ProviderRefused(provider, refusal)
  window.location.assign(data.url)
}

/** Ends this browser's session, leaving the person's other devices signed in. */
export async function signOut(): Promise<void> {
  const { error } = await browserAuth().signOut({ scope: 'local' })
  if (error) throw error
}

/** Why a session stopped: it ran out, or the API would not take its token. */
type SessionEnd = 'ended' | 'refused'

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

/** The access token of the request's session, read after hasSession has verified and refreshed it. */
export async function sessionToken(session: ServerSession): Promise<string | null> {
  const { data } = await serverAuth(session).getSession()
  return data.session?.access_token ?? null
}

/** Trades the code the provider sent back for a session, written through its cookie methods. */
export async function completeSignIn(session: ServerSession, code: string): Promise<boolean> {
  const { error } = await serverAuth(session).exchangeCodeForSession(code)
  return error == null
}
