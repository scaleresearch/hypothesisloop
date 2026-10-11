import { render, screen } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { hasSession } from '@/lib/auth'
import LandingPage from '@/app/page'

const request = vi.hoisted(() => ({ headers: new Headers(), cookies: [{ name: 'sb-auth-token', value: 'session' }] }))

vi.mock('next/headers', () => ({ headers: () => request.headers, cookies: () => ({ getAll: () => request.cookies }) }))
vi.mock('@/lib/auth', async importActual => ({
  ...(await importActual<typeof import('@/lib/auth')>()),
  hasSession: vi.fn(),
}))

beforeEach(() => {
  request.headers = new Headers({ host: 'ui.test' })
})

async function show(signedIn: boolean) {
  vi.mocked(hasSession).mockResolvedValue(signedIn)
  render(await LandingPage())
  return screen.getAllByRole('link').map(link => [link.textContent, link.getAttribute('href')])
}

describe('the landing page', () => {
  it('offers a visitor without a session sign-in and sign-up, which are the same page worded twice', async () => {
    expect(await show(false)).toEqual([['Sign in', '/login'], ['Sign up', '/login?mode=sign-up']])
  })

  it('offers a signed-in visitor the console and nothing else', async () => {
    expect(await show(true)).toEqual([['Console', '/console']])
  })

  it('checks the session with the request\'s cookies and scheme, and writes none', async () => {
    request.headers = new Headers({ host: 'ui.test', 'x-forwarded-proto': 'https' })
    await show(false)
    const session = vi.mocked(hasSession).mock.calls[0][0]
    expect(session.https).toBe(true)
    expect(session.cookies.getAll()).toEqual(request.cookies)
  })
})
