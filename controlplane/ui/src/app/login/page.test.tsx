import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { ProviderRefused, signIn } from '@/lib/auth'
import LoginPage from '@/app/login/page'

const navigation = vi.hoisted(() => ({ search: '' }))

vi.mock('next/navigation', () => ({ useSearchParams: () => new URLSearchParams(navigation.search) }))
vi.mock('@/lib/auth', async importActual => ({
  ...(await importActual<typeof import('@/lib/auth')>()),
  signIn: vi.fn(),
}))

beforeEach(() => {
  navigation.search = ''
  vi.mocked(signIn).mockResolvedValue()
})

describe('the sign-in page', () => {
  it('has one button for each provider the configuration lists, each with its mark', () => {
    render(<LoginPage />)
    const buttons = screen.getAllByRole('button')
    expect(buttons.map(b => b.textContent)).toEqual(['Continue with GitHub', 'Continue with Google', 'Continue with Apple'])
    for (const button of buttons) expect(button.querySelector('svg[aria-hidden="true"]')).not.toBeNull()
  })

  it('offers only what the configuration lists', () => {
    vi.stubEnv('NEXT_PUBLIC_SIGN_IN_PROVIDERS', 'github')
    render(<LoginPage />)
    expect(screen.getAllByRole('button').map(b => b.textContent)).toEqual(['Continue with GitHub'])
    vi.unstubAllEnvs()
  })

  it('says what is missing, and offers nothing, when no provider is configured', () => {
    vi.stubEnv('NEXT_PUBLIC_SIGN_IN_PROVIDERS', '')
    render(<LoginPage />)
    expect(screen.getByRole('alert').textContent).toContain('Sign-in is not configured. NEXT_PUBLIC_SIGN_IN_PROVIDERS is not set')
    expect(screen.queryByRole('button')).toBeNull()
    vi.unstubAllEnvs()
  })

  it('is worded for signing in, and offers the sign-up wording with the same way back', () => {
    navigation.search = 'next=%2Fjobs'
    render(<LoginPage />)
    expect(screen.getByRole('heading', { name: 'Sign in' })).toBeTruthy()
    expect(screen.getByRole('link', { name: 'Sign up' }).getAttribute('href')).toBe('/login?next=%2Fjobs&mode=sign-up')
  })

  it('is worded for signing up when asked, with the same buttons, and offers the way back to signing in', () => {
    navigation.search = 'mode=sign-up'
    render(<LoginPage />)
    expect(screen.getByRole('heading', { name: 'Create your account' })).toBeTruthy()
    expect(screen.getAllByRole('button').map(b => b.textContent)).toEqual(['Continue with GitHub', 'Continue with Google', 'Continue with Apple'])
    expect(screen.getByRole('link', { name: 'Sign in' }).getAttribute('href')).toBe('/login')
  })

  it('leads back to the landing page by the logo', () => {
    render(<LoginPage />)
    expect(screen.getByRole('link', { name: 'HypothesisLoop' }).getAttribute('href')).toBe('/')
  })

  it('starts GitHub sign-in with the way back the URL names', async () => {
    navigation.search = 'next=%2Fjobs%3Fstatus%3DRUNNING'
    render(<LoginPage />)
    await userEvent.click(screen.getByRole('button', { name: 'Continue with GitHub' }))
    expect(signIn).toHaveBeenCalledExactlyOnceWith('github', '/jobs?status=RUNNING')
  })

  it('goes home afterwards when the way back is another site', async () => {
    navigation.search = 'next=https%3A%2F%2Fevil.example'
    render(<LoginPage />)
    await userEvent.click(screen.getByRole('button', { name: 'Continue with GitHub' }))
    expect(signIn).toHaveBeenCalledExactlyOnceWith('github', '/console')
  })

  it.each([
    ['ended', 'Your session ended. Sign in again to continue where you were.'],
    ['refused', 'The API did not accept your session. Sign in again.'],
    ['failed', 'Sign-in did not complete. Try again.'],
  ])('says why the visitor is back: %s', (notice, text) => {
    navigation.search = `notice=${notice}`
    render(<LoginPage />)
    expect(screen.getByText(text)).toBeTruthy()
  })

  it.each(['%3Cscript%3E', '__proto__', 'constructor', 'toString'])('shows nothing for a notice it does not know: %s', notice => {
    navigation.search = `notice=${notice}`
    render(<LoginPage />)
    expect(document.querySelector('.error-banner')).toBeNull()
    expect(screen.getByRole('button', { name: 'Continue with GitHub' })).toBeTruthy()
  })

  it.each(['/%09/evil.example', '/%0A/evil.example', '/%5Cevil.example'])('goes home afterwards when the way back hides another site: %s', async next => {
    navigation.search = `next=${next}`
    render(<LoginPage />)
    await userEvent.click(screen.getByRole('button', { name: 'Continue with GitHub' }))
    expect(signIn).toHaveBeenCalledExactlyOnceWith('github', '/console')
  })

  it('shows why sign-in could not start and lets the visitor try again', async () => {
    vi.mocked(signIn).mockRejectedValue(new Error('provider is not enabled'))
    render(<LoginPage />)
    await userEvent.click(screen.getByRole('button', { name: 'Continue with GitHub' }))
    expect(await screen.findByText('Sign-in could not start: provider is not enabled')).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Continue with GitHub' })).toHaveProperty('disabled', false)
  })

  it('says a provider is not available when the identity provider refuses it, and leaves the others to choose', async () => {
    vi.mocked(signIn).mockRejectedValue(new ProviderRefused('google', 'Unsupported provider: provider is not enabled'))
    render(<LoginPage />)
    await userEvent.click(screen.getByRole('button', { name: 'Continue with Google' }))
    expect((await screen.findByRole('alert')).textContent).toBe('Sign-in with Google is not available yet. Choose another way to sign in.')
    expect(screen.getByRole('button', { name: 'Continue with Google' })).toHaveProperty('disabled', true)
    expect(screen.getByRole('button', { name: 'Continue with GitHub' })).toHaveProperty('disabled', false)
    expect(screen.getByRole('button', { name: 'Continue with Apple' })).toHaveProperty('disabled', false)
  })
})
