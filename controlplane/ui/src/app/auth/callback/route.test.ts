// @vitest-environment node
import { NextRequest } from 'next/server'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { completeSignIn } from '@/lib/auth'
import { GET } from '@/app/auth/callback/route'

vi.mock('@/lib/auth', async importActual => ({
  ...(await importActual<typeof import('@/lib/auth')>()),
  completeSignIn: vi.fn(),
}))

function arrive(query: string) {
  return GET(new NextRequest(`http://ui.test/auth/callback${query}`, { headers: { cookie: 'sb-code-verifier=v1' } }))
}

beforeEach(() => {
  vi.mocked(completeSignIn).mockImplementation(async (cookies, code) => {
    if (code !== 'good') return false
    await cookies.setAll!([{ name: 'sb-auth-token', value: 'session', options: { path: '/' } }], { 'Cache-Control': 'private, no-store' })
    return true
  })
})

describe('the sign-in callback', () => {
  it('signs the visitor in and sends them to where they were going', async () => {
    const response = await arrive('?code=good&next=%2Fjobs%3Fstatus%3DRUNNING')
    expect(response.status).toBe(303)
    expect(response.headers.get('location')).toBe('/jobs?status=RUNNING')
    expect(response.cookies.get('sb-auth-token')?.value).toBe('session')
    expect(response.headers.get('cache-control')).toBe('private, no-store')
  })

  it('hands the exchange the request\'s cookies, where the code verifier is', async () => {
    await arrive('?code=good')
    const [cookies] = vi.mocked(completeSignIn).mock.calls[0]
    expect(await cookies.getAll()).toEqual([{ name: 'sb-code-verifier', value: 'v1' }])
  })

  it('sends them home when no way back is named, or it leads to another site', async () => {
    expect((await arrive('?code=good')).headers.get('location')).toBe('/')
    expect((await arrive('?code=good&next=https%3A%2F%2Fevil.example')).headers.get('location')).toBe('/')
  })

  it('returns to sign-in, saying so, when the code is refused', async () => {
    const response = await arrive('?code=stale&next=%2Fjobs')
    expect(response.headers.get('location')).toBe('/login?notice=failed')
    expect(response.cookies.get('sb-auth-token')).toBeUndefined()
  })

  it('returns to sign-in when the provider sent an error and no code', async () => {
    const response = await arrive('?error=access_denied&error_description=The+user+denied+access')
    expect(response.headers.get('location')).toBe('/login?notice=failed')
    expect(completeSignIn).not.toHaveBeenCalled()
  })
})
