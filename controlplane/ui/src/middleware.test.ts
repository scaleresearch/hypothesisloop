// @vitest-environment node
import { NextRequest } from 'next/server'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { hasSession } from '@/lib/auth'
import { config, middleware } from '@/middleware'

vi.mock('@/lib/auth', async importActual => ({
  ...(await importActual<typeof import('@/lib/auth')>()),
  hasSession: vi.fn(),
}))

function visit(path: string, cookie?: string) {
  return middleware(new NextRequest(`http://ui.test${path}`, cookie ? { headers: { cookie } } : undefined))
}

function signedIn(value: boolean) {
  vi.mocked(hasSession).mockResolvedValue(value)
}

beforeEach(() => signedIn(false))

describe('a visitor without a session', () => {
  it('is sent to sign in, with the page they asked for as the way back', async () => {
    const response = await visit('/jobs?status=RUNNING&project=prj_1')
    expect(response.status).toBe(307)
    expect(response.headers.get('location')).toBe('http://ui.test/login?next=%2Fjobs%3Fstatus%3DRUNNING%26project%3Dprj_1')
  })

  it('reaches the landing page', async () => {
    const response = await visit('/')
    expect(response.status).toBe(200)
    expect(response.headers.get('location')).toBeNull()
  })

  it('is sent to sign in from the console and from a record', async () => {
    expect((await visit('/console')).headers.get('location')).toBe('http://ui.test/login?next=%2Fconsole')
    expect((await visit('/platform-experiments/pe-1')).headers.get('location')).toBe('http://ui.test/login?next=%2Fplatform-experiments%2Fpe-1')
  })

  it('reaches the invite page, which keeps the token of its link before it sends them to sign in', async () => {
    const response = await visit('/invite')
    expect(response.status).toBe(200)
    expect(response.headers.get('location')).toBeNull()
    expect((await visit('/invite/more')).headers.get('location')).toBe('http://ui.test/login?next=%2Finvite%2Fmore')
  })

  it('reaches the sign-in page itself', async () => {
    const response = await visit('/login?next=%2Fjobs')
    expect(response.status).toBe(200)
    expect(response.headers.get('location')).toBeNull()
  })
})

describe('a signed-in visitor', () => {
  beforeEach(() => signedIn(true))

  it('reaches the landing page too, which offers them the console', async () => {
    const response = await visit('/')
    expect(response.status).toBe(200)
    expect(response.headers.get('location')).toBeNull()
  })

  it('reaches the page', async () => {
    const response = await visit('/jobs')
    expect(response.status).toBe(200)
    expect(response.headers.get('x-middleware-next')).toBe('1')
  })

  it('is sent on from the sign-in page to the invite they opened', async () => {
    expect((await visit('/login?next=%2Finvite')).headers.get('location')).toBe('http://ui.test/invite')
  })

  it('is sent on from the sign-in page to where they were going', async () => {
    const response = await visit('/login?next=%2Fjobs%3Fstatus%3DRUNNING')
    expect(response.headers.get('location')).toBe('http://ui.test/jobs?status=RUNNING')
  })

  it('is sent home from the sign-in page when the way back is another site', async () => {
    expect((await visit('/login?next=https%3A%2F%2Fevil.example')).headers.get('location')).toBe('http://ui.test/console')
    expect((await visit('/login')).headers.get('location')).toBe('http://ui.test/console')
  })

  it('is sent home when the way back hides another site behind a tab, a newline or a backslash', async () => {
    for (const hostile of ['/%09/evil.example', '/%0A/evil.example', '/%0D/evil.example', '%09//evil.example', '/%5Cevil.example', '%5C%5Cevil.example']) {
      expect((await visit(`/login?next=${hostile}`)).headers.get('location')).toBe('http://ui.test/console')
    }
  })
})

describe('the scheme the visitor came over', () => {
  it('reaches the session check, which marks its cookies Secure over https', async () => {
    await middleware(new NextRequest('http://ui.internal/jobs', { headers: { 'x-forwarded-proto': 'https' } }))
    expect(vi.mocked(hasSession).mock.lastCall![0].https).toBe(true)
    await visit('/jobs')
    expect(vi.mocked(hasSession).mock.lastCall![0].https).toBe(false)
  })
})

describe('cookies the session check writes', () => {
  const refreshed = [{ name: 'sb-auth-token', value: 'fresh', options: { path: '/', httpOnly: false } }]
  const cacheHeaders = { 'Cache-Control': 'private, no-store' }

  it('leave with the page when a token was refreshed', async () => {
    vi.mocked(hasSession).mockImplementation(async ({ cookies }) => {
      await cookies.setAll!(refreshed, cacheHeaders)
      return true
    })
    const response = await visit('/jobs', 'sb-auth-token=stale')
    expect(response.cookies.get('sb-auth-token')?.value).toBe('fresh')
    expect(response.headers.get('cache-control')).toBe('private, no-store')
    // The page being rendered sees the refreshed cookie too.
    expect(response.headers.get('x-middleware-request-cookie')).toContain('sb-auth-token=fresh')
  })

  it('leave with the redirect when a dead session was cleared', async () => {
    vi.mocked(hasSession).mockImplementation(async ({ cookies }) => {
      await cookies.setAll!([{ name: 'sb-auth-token', value: '', options: { path: '/', maxAge: 0 } }], cacheHeaders)
      return false
    })
    const response = await visit('/jobs', 'sb-auth-token=dead')
    expect(response.headers.get('location')).toBe('http://ui.test/login?next=%2Fjobs')
    expect(response.headers.get('set-cookie')).toMatch(/sb-auth-token=;.*Max-Age=0/)
  })

  it('reads the request\'s cookies', async () => {
    let seen: unknown
    vi.mocked(hasSession).mockImplementation(async ({ cookies }) => {
      seen = await cookies.getAll()
      return true
    })
    await visit('/jobs', 'sb-auth-token=abc')
    expect(seen).toEqual([{ name: 'sb-auth-token', value: 'abc' }])
  })
})

describe('an identity provider that cannot be reached', () => {
  it('is an error, never a silent sign-out', async () => {
    vi.mocked(hasSession).mockRejectedValue(new Error('fetch failed'))
    await expect(visit('/jobs')).rejects.toThrow('fetch failed')
  })
})

describe('the paths the middleware runs on', () => {
  const matcher = new RegExp(`^${config.matcher[0]}$`)

  it('are every page', () => {
    for (const path of ['/', '/login', '/console', '/jobs', '/platform-experiments/pe-1', '/settings']) expect(path).toMatch(matcher)
  })

  it('leave out the sign-in callback and the framework\'s own assets', () => {
    for (const path of ['/auth/callback', '/_next/static/chunk.js', '/_next/image', '/favicon.ico']) expect(path).not.toMatch(matcher)
  })
})
