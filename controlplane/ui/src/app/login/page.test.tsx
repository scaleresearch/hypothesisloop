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
