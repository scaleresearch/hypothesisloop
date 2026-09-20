import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { signIn } from '@/lib/auth'
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
  it('has one button', () => {
    render(<LoginPage />)
    expect(screen.getAllByRole('button').map(b => b.textContent)).toEqual(['Continue with GitHub'])
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
    expect(signIn).toHaveBeenCalledExactlyOnceWith('github', '/')
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

  it('shows nothing for a notice it does not know', () => {
    navigation.search = 'notice=%3Cscript%3E'
    render(<LoginPage />)
    expect(document.querySelector('.error-banner')).toBeNull()
  })

  it('shows why sign-in could not start and lets the visitor try again', async () => {
    vi.mocked(signIn).mockRejectedValue(new Error('provider is not enabled'))
    render(<LoginPage />)
    await userEvent.click(screen.getByRole('button', { name: 'Continue with GitHub' }))
    expect(await screen.findByText('Sign-in could not start: provider is not enabled')).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Continue with GitHub' })).toHaveProperty('disabled', false)
  })
})
