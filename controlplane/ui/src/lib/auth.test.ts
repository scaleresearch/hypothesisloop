import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createBrowserClient, createServerClient } from '@supabase/ssr'
import { AuthRetryableFetchError, AuthApiError } from '@supabase/supabase-js'
import { SIGN_IN_METHODS, accessToken, completeSignIn, hasSession, overHTTPS, safeNext, signIn, signOut } from '@/lib/auth'

vi.mock('@supabase/ssr', () => ({ createBrowserClient: vi.fn(), createServerClient: vi.fn() }))

const auth = {
  signInWithOAuth: vi.fn(),
  signOut: vi.fn(),
  getSession: vi.fn(),
  getClaims: vi.fn(),
  exchangeCodeForSession: vi.fn(),
}
const cookies = { getAll: () => [], setAll: () => {} }
const session = { cookies, https: false }

beforeEach(() => {
  vi.mocked(createBrowserClient).mockReturnValue({ auth } as never)
  vi.mocked(createServerClient).mockReturnValue({ auth } as never)
})

describe('the client', () => {
  it('is built from the two public variables and stores the session in cookies alone', async () => {
    auth.getSession.mockResolvedValue({ data: { session: null }, error: null })
    await accessToken()
    // No storage option is passed: the library's own cookie storage is the only one.
    expect(createBrowserClient).toHaveBeenCalledWith('http://identity.test', 'sb_publishable_test', { cookieOptions: { secure: false } })
  })

  it('refuses to start without its configuration', async () => {
    vi.stubEnv('NEXT_PUBLIC_SUPABASE_URL', '')
    await expect(accessToken()).rejects.toThrow('NEXT_PUBLIC_SUPABASE_URL is not set')
    vi.stubEnv('NEXT_PUBLIC_SUPABASE_URL', 'http://identity.test')
    vi.stubEnv('NEXT_PUBLIC_SUPABASE_PUBLISHABLE_KEY', '')
    await expect(accessToken()).rejects.toThrow('NEXT_PUBLIC_SUPABASE_PUBLISHABLE_KEY is not set')
    vi.unstubAllEnvs()
  })
})

describe('signing in', () => {
  it('offers GitHub and nothing else', () => {
    expect(SIGN_IN_METHODS).toEqual([{ provider: 'github', label: 'Continue with GitHub' }])
  })

  it('sends the browser to the provider and names the callback with the way back', async () => {
    auth.signInWithOAuth.mockResolvedValue({ data: {}, error: null })
    await signIn('github', '/jobs?status=RUNNING')
    expect(auth.signInWithOAuth).toHaveBeenCalledExactlyOnceWith({
      provider: 'github',
      options: { redirectTo: `${window.location.origin}/auth/callback?next=%2Fjobs%3Fstatus%3DRUNNING` },
    })
  })

  it('fails loudly when the provider cannot be started', async () => {
    auth.signInWithOAuth.mockResolvedValue({ data: {}, error: new Error('provider is not enabled') })
    await expect(signIn('github', '/')).rejects.toThrow('provider is not enabled')
  })

  it('returns only to a path of this app', () => {
    expect(safeNext('/jobs?status=RUNNING')).toBe('/jobs?status=RUNNING')
    expect(safeNext(null)).toBe('/')
    expect(safeNext('')).toBe('/')
    expect(safeNext('https://evil.example/')).toBe('/')
    expect(safeNext('//evil.example/')).toBe('/')
    expect(safeNext('/\\evil.example/')).toBe('/')
    expect(safeNext('\\\\evil.example/')).toBe('/')
    expect(safeNext('javascript:alert(1)')).toBe('/')
    expect(safeNext('http://')).toBe('/')
  })

  it('reads the way back as a browser would, which drops tabs and newlines before resolving', () => {
    for (const hostile of ['/\t/evil.example', '/\n/evil.example', '/\r/evil.example', '\t//evil.example', '/\t\\evil.example']) {
      expect(safeNext(hostile)).toBe('/')
    }
    for (const encoded of ['/%09/evil.example', '/%0A/evil.example']) {
      expect(safeNext(decodeURIComponent(encoded))).toBe('/')
      // Still encoded, it is a path of this app and stays one.
      expect(safeNext(encoded)).toBe(encoded)
    }
  })

  it('keeps the query and fragment of a way back, and resolves a relative one inside this app', () => {
    expect(safeNext('/jobs?status=RUNNING#top')).toBe('/jobs?status=RUNNING#top')
    expect(safeNext('jobs')).toBe('/jobs')
    expect(safeNext('/a/../jobs')).toBe('/jobs')
  })

  it('trades the callback code for a session', async () => {
    auth.exchangeCodeForSession.mockResolvedValue({ data: {}, error: null })
    await expect(completeSignIn(session, 'code-1')).resolves.toBe(true)
    expect(auth.exchangeCodeForSession).toHaveBeenCalledExactlyOnceWith('code-1')
    expect(createServerClient).toHaveBeenCalledWith('http://identity.test', 'sb_publishable_test', { cookies, cookieOptions: { secure: false } })
  })

  it('reports a code the provider does not take', async () => {
    auth.exchangeCodeForSession.mockResolvedValue({ data: {}, error: new Error('invalid grant') })
    await expect(completeSignIn(session, 'stale')).resolves.toBe(false)
  })
})

describe('signing out', () => {
  it('ends this browser\'s session only', async () => {
    auth.signOut.mockResolvedValue({ error: null })
    await signOut()
    expect(auth.signOut).toHaveBeenCalledExactlyOnceWith({ scope: 'local' })
  })
})

describe('the access token', () => {
  it('is the session\'s, as the library hands it out after any refresh', async () => {
    auth.getSession.mockResolvedValue({ data: { session: { access_token: 'jwt-1' } }, error: null })
    await expect(accessToken()).resolves.toBe('jwt-1')
  })

  it('is null without a session', async () => {
    auth.getSession.mockResolvedValue({ data: { session: null }, error: null })
    await expect(accessToken()).resolves.toBeNull()
  })

  it('is null when the session can no longer be refreshed', async () => {
    auth.getSession.mockResolvedValue({
      data: { session: null }, error: new AuthApiError('Invalid Refresh Token', 400, 'refresh_token_not_found'),
    })
    await expect(accessToken()).resolves.toBeNull()
  })

  it('is an error, and no sign-out, when the identity provider cannot be reached', async () => {
    auth.getSession.mockResolvedValue({ data: { session: null }, error: new AuthRetryableFetchError('fetch failed', 0) })
    await expect(accessToken()).rejects.toThrow('fetch failed')
  })
})

describe('session cookies', () => {
  it('are marked Secure for a visitor who came over https', async () => {
    auth.getClaims.mockResolvedValue({ data: null, error: null })
    await hasSession({ cookies, https: true })
    expect(createServerClient).toHaveBeenLastCalledWith('http://identity.test', 'sb_publishable_test', { cookies, cookieOptions: { secure: true } })
  })

  it('take the scheme from the proxy that ended TLS, and from the request without one', () => {
    const url = (s: string) => new URL(s)
    expect(overHTTPS(new Headers({ 'x-forwarded-proto': 'https' }), url('http://ui.internal/jobs'))).toBe(true)
    expect(overHTTPS(new Headers({ 'x-forwarded-proto': 'https, http' }), url('http://ui.internal/jobs'))).toBe(true)
    expect(overHTTPS(new Headers({ 'x-forwarded-proto': 'http' }), url('https://ui.internal/jobs'))).toBe(false)
    expect(overHTTPS(new Headers(), url('https://ui.test/jobs'))).toBe(true)
    expect(overHTTPS(new Headers(), url('http://localhost:3000/jobs'))).toBe(false)
  })
})

describe('the server-side session check', () => {
  it('holds for a token that verifies', async () => {
    auth.getClaims.mockResolvedValue({ data: { claims: { sub: 'u1' } }, error: null })
    await expect(hasSession(session)).resolves.toBe(true)
  })

  it('fails without a session', async () => {
    auth.getClaims.mockResolvedValue({ data: null, error: null })
    await expect(hasSession(session)).resolves.toBe(false)
  })

  it('fails for a forged or stale token', async () => {
    auth.getClaims.mockResolvedValue({ data: null, error: new Error('Invalid JWT signature') })
    await expect(hasSession(session)).resolves.toBe(false)
  })

  it('is an error when the identity provider cannot be reached', async () => {
    auth.getClaims.mockResolvedValue({ data: null, error: new AuthRetryableFetchError('fetch failed', 0) })
    await expect(hasSession(session)).rejects.toThrow('fetch failed')
  })
})
