import { render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { SIGNED_IN_HEADER } from '@/lib/auth'
import LandingPage from '@/app/page'

const request = vi.hoisted(() => ({ headers: new Headers() }))

vi.mock('next/headers', () => ({ headers: () => request.headers }))

function show(signedIn: string | null) {
  request.headers = new Headers(signedIn === null ? {} : { [SIGNED_IN_HEADER]: signedIn })
  render(LandingPage())
  return screen.getAllByRole('link').map(link => [link.textContent, link.getAttribute('href')])
}

describe('the landing page', () => {
  it('offers a visitor without a session sign-in and sign-up, which are the same page worded twice', () => {
    expect(show('no')).toEqual([['Sign in', '/login'], ['Sign up', '/login?mode=sign-up']])
  })

  it('offers a signed-in visitor the console and nothing else', () => {
    expect(show('yes')).toEqual([['Console', '/console']])
  })

  it('takes a request the middleware did not answer for as signed out', () => {
    expect(show(null)).toEqual([['Sign in', '/login'], ['Sign up', '/login?mode=sign-up']])
  })
})
