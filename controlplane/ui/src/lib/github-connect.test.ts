import { createElement, type ReactNode } from 'react'
import { act, renderHook, waitFor } from '@testing-library/react'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { beginGitHubInstall } from '@/lib/api/github'
import type { OrgGitHubInstallation } from '@/types/api'

vi.mock('@/lib/auth', () => ({ accessToken: vi.fn(), endSession: vi.fn() }))
vi.mock('@/lib/api/github', async importActual => ({
  ...(await importActual<typeof import('@/lib/api/github')>()),
  beginGitHubInstall: vi.fn(),
}))

type Module = typeof import('@/lib/github-connect')
// Read after each module reset, so that it is the class the module under test checks against.
let ApiError: typeof import('@/lib/api/request').ApiError

const INSTALL = 'https://github.test/apps/hl/installations/new'
const RETURN = '/projects/prj_1/experiments?new=1'
const installation: OrgGitHubInstallation = { org_id: 'org_a', installation_id: 7, account_login: 'acme', account_type: 'Organization', connected_actor: { kind: 'user', id: 'u1', name: 'Ada' }, connected_at: '2026-01-01T00:00:00Z', default: true }

let lib: Module
let finish: ReturnType<typeof vi.fn>

// What every other tab of this origin hears on the install channel.
function overheard() {
  const heard: unknown[] = []
  const channel = new BroadcastChannel('hl-github-install')
  channel.onmessage = event => heard.push(event.data)
  return { heard, close: () => channel.close() }
}

function tab() {
  return { location: { href: '' }, close: vi.fn() }
}

beforeEach(async () => {
  vi.resetModules()
  lib = await import('@/lib/github-connect')
  ApiError = (await import('@/lib/api/request')).ApiError
  finish = vi.fn().mockResolvedValue({ org_id: 'org_a', return_to: RETURN, installation })
})

describe('opening the install', () => {
  it('opens the tab inside the click, then sends it to the install page carrying the state', async () => {
    const opened = tab()
    let begun = false
    const open = vi.fn(() => {
      expect(begun).toBe(false)
      return opened
    })
    const begin = vi.fn(async () => {
      begun = true
      return { state: 's-1', install_url: `${INSTALL}?state=s-1`, expires_at: '' }
    })
    const started = lib.openInstall('org_a', RETURN, { open, begin })
    expect(open).toHaveBeenCalledTimes(1)
    await expect(started).resolves.toBe('s-1')
    expect(begin).toHaveBeenCalledExactlyOnceWith('org_a', RETURN)
    expect(opened.location.href).toBe(`${INSTALL}?state=s-1`)
  })

  it('says how to allow the tab when the browser blocks it, and asks the API nothing', async () => {
    const begin = vi.fn()
    await expect(lib.openInstall('org_a', RETURN, { open: () => null, begin })).rejects.toThrow('Allow pop-ups for this site')
    expect(begin).not.toHaveBeenCalled()
  })

  it('closes the tab again when the API refuses the install', async () => {
    const opened = tab()
    await expect(lib.openInstall('org_a', RETURN, { open: () => opened, begin: () => Promise.reject(new Error('not an owner')) })).rejects.toThrow('not an owner')
    expect(opened.close).toHaveBeenCalledTimes(1)
  })
})

describe('the tab GitHub returns to', () => {
  it('connects in one pass with the state and the installation, and tells the tab the install started in', async () => {
    const other = overheard()
    await expect(lib.takeReturn('installation_id=7&setup_action=install&state=s-1&code=c-1', finish)).resolves.toEqual({ view: 'connected', account: 'acme', returnTo: RETURN })
    expect(finish).toHaveBeenCalledExactlyOnceWith('s-1', 'c-1', 7)
    await waitFor(() => expect(other.heard).toEqual([{ state: 's-1', outcome: { ok: true, installation } }]))
    other.close()
  })

  it('connects once when the page asks twice, as React does in development', async () => {
    const first = lib.takeReturn('installation_id=7&setup_action=install&state=s-1&code=c-1', finish)
    const second = lib.takeReturn('installation_id=7&setup_action=install&state=s-1&code=c-1', finish)
    expect(await second).toEqual(await first)
    expect(finish).toHaveBeenCalledTimes(1)
  })

  it('tells the refusal of an install the person may not connect, in the API\'s words and reason', async () => {
    const other = overheard()
    finish.mockRejectedValue(new ApiError(422, 'This return from GitHub does not match an install you started.', 'install_state_invalid'))
    await expect(lib.takeReturn('installation_id=7&setup_action=install&state=forged', finish)).resolves.toEqual({ view: 'failed', message: 'This return from GitHub does not match an install you started.' })
    await waitFor(() => expect(other.heard).toEqual([{ state: 'forged', outcome: { ok: false, message: 'This return from GitHub does not match an install you started.', reason: 'install_state_invalid' } }]))
    other.close()
  })

  it('says an owner of the organization was asked to approve, and connects nothing', async () => {
    const view = await lib.takeReturn('setup_action=request&state=s-1', finish)
    expect(view).toEqual({ view: 'failed', message: expect.stringContaining('asked an owner of that organization to approve') })
    expect(finish).not.toHaveBeenCalled()
  })

  it('reads a change made on GitHub alone, with no state, as a change and connects nothing', async () => {
    const other = overheard()
    await expect(lib.takeReturn('installation_id=7&setup_action=update', finish)).resolves.toEqual({ view: 'changed' })
    await waitFor(() => expect(other.heard).toEqual([{ changed: true }]))
    expect(finish).not.toHaveBeenCalled()
    other.close()
  })

  it('reads a visit as nothing', async () => {
    await expect(lib.takeReturn('', finish)).resolves.toEqual({ view: 'nothing' })
    expect(finish).not.toHaveBeenCalled()
  })

  it('leads back to no page outside this app', async () => {
    finish.mockResolvedValue({ org_id: 'org_a', return_to: '//elsewhere.example', installation })
    await expect(lib.takeReturn('installation_id=7&state=s-2', finish)).resolves.toMatchObject({ view: 'connected', returnTo: null })
  })
})

describe('the tab the install started in', () => {
  const opened = tab()
  function hook() {
    vi.spyOn(window, 'open').mockReturnValue(opened as unknown as Window)
    vi.mocked(beginGitHubInstall).mockResolvedValue({ state: 's-9', install_url: `${INSTALL}?state=s-9`, expires_at: '' })
    const cache = new Map()
    const wrapper = ({ children }: { children: ReactNode }) => createElement(SWRConfig, { value: { provider: () => cache } }, children)
    return renderHook(() => lib.useGitHubInstall('org_a', RETURN), { wrapper })
  }

  it('waits for GitHub, then shows the account the other tab connected', async () => {
    const { result } = hook()
    act(() => result.current.start())
    await waitFor(() => expect(result.current.install).toEqual({ phase: 'waiting', state: 's-9' }))
    expect(beginGitHubInstall).toHaveBeenCalledExactlyOnceWith('org_a', RETURN)

    // Another install's outcome says nothing here.
    await lib.takeReturn('installation_id=8&state=s-other', finish)
    await lib.takeReturn('installation_id=7&state=s-9', finish)
    await waitFor(() => expect(result.current.install).toEqual({ phase: 'connected', installation }))
  })

  it('shows the precise refusal the other tab was given', async () => {
    finish.mockRejectedValue(new ApiError(422, 'GitHub does not list this installation among those the GitHub account that finished the install may reach.', 'installation_not_authorized'))
    const { result } = hook()
    act(() => result.current.start())
    await waitFor(() => expect(result.current.install.phase).toBe('waiting'))
    await lib.takeReturn('installation_id=7&state=s-9', finish)
    await waitFor(() => expect(result.current.install).toEqual({ phase: 'failed', message: 'GitHub does not list this installation among those the GitHub account that finished the install may reach.', reason: 'installation_not_authorized' }))
  })

  it('stops waiting when the person cancels, and ignores a late outcome', async () => {
    const { result } = hook()
    act(() => result.current.start())
    await waitFor(() => expect(result.current.install.phase).toBe('waiting'))
    act(() => result.current.reset())
    await lib.takeReturn('installation_id=7&state=s-9', finish)
    await new Promise(resolve => setTimeout(resolve, 20))
    expect(result.current.install).toEqual({ phase: 'idle' })
  })

  it('says the tab was blocked without asking the API', async () => {
    const { result } = hook()
    vi.mocked(window.open).mockReturnValue(null)
    act(() => result.current.start())
    await waitFor(() => expect(result.current.install).toMatchObject({ phase: 'failed', message: expect.stringContaining('Allow pop-ups') }))
    expect(beginGitHubInstall).not.toHaveBeenCalled()
  })
})

describe('the installation page on GitHub', () => {
  it('is the organization\'s settings for an organization and the person\'s own for a personal account', () => {
    expect(lib.installationSettingsURL({ installation_id: 7, account_login: 'acme', account_type: 'Organization' })).toBe('https://github.com/organizations/acme/settings/installations/7')
    expect(lib.installationSettingsURL({ installation_id: 8, account_login: 'ada', account_type: 'User' })).toBe('https://github.com/settings/installations/8')
  })
})
