import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { GitHubInstallation } from '@/types'

vi.mock('@/lib/auth', () => ({ accessToken: vi.fn(), endSession: vi.fn() }))

type Module = typeof import('@/lib/github-connect')

const AUTHORIZE = 'https://github.test/login/oauth/authorize?client_id=app'
const INSTALL = 'https://github.test/apps/hl/installations/new'
const origin = { orgID: 'org_a', projectID: 'prj_1' }
const installation: GitHubInstallation = { org_id: 'org_a', installation_id: 7, account_login: 'acme', connected_actor: { kind: 'user', id: 'u1', name: 'Ada' }, connected_at: '2026-01-01T00:00:00Z' }

let lib: Module
// Loaded with the module under test, so its refusals are the class that module checks for.
let ApiError: typeof import('@/lib/api').ApiError
let connect: ReturnType<typeof vi.fn>
let app: ReturnType<typeof vi.fn>
let state: () => ReturnType<Module['useConnect']>

// The state the module holds, read the way a component reads it but without rendering one.
async function load() {
  vi.resetModules()
  lib = await import('@/lib/github-connect')
  ApiError = (await import('@/lib/api')).ApiError
  const { renderHook } = await import('@testing-library/react')
  const hook = renderHook(() => lib.useConnect())
  state = () => hook.result.current
}

function returned(query: string) {
  return lib.takeReturn(query, { app, connect } as never)
}

function sentState(address: string) {
  return new URL(address).searchParams.get('state')!
}

beforeEach(async () => {
  window.sessionStorage.clear()
  connect = vi.fn().mockResolvedValue(installation)
  app = vi.fn().mockResolvedValue({ authorize_url: AUTHORIZE, install_url: INSTALL })
  await load()
})

describe('leaving to authorise', () => {
  it('names where GitHub returns to and a state no one else knows', () => {
    const address = new URL(lib.beginConnect(AUTHORIZE, origin))
    expect(address.origin + address.pathname).toBe('https://github.test/login/oauth/authorize')
    expect(address.searchParams.get('client_id')).toBe('app')
    expect(address.searchParams.get('redirect_uri')).toBe(`${window.location.origin}/settings/repository`)
    expect(address.searchParams.get('state')).toMatch(/^[0-9a-f-]{36}$/)
    expect(sentState(lib.beginConnect(AUTHORIZE, origin))).not.toBe(address.searchParams.get('state'))
  })
})

describe('a return from authorising', () => {
  it('connects once with the code, and leads back to the project the person was in', async () => {
    const nonce = sentState(lib.beginConnect(AUTHORIZE, origin))
    await expect(returned(`code=code-1&state=${nonce}`)).resolves.toEqual({ to: 'settings', projectID: 'prj_1' })
    expect(connect).toHaveBeenCalledExactlyOnceWith('org_a', 'code-1', undefined)
    expect(state()).toEqual({ state: 'connected', orgID: 'org_a', installation })
  })

  it('is taken once when the page asks twice, as React does in development', async () => {
    const nonce = sentState(lib.beginConnect(AUTHORIZE, origin))
    const first = returned(`code=code-1&state=${nonce}`)
    const second = returned(`code=code-1&state=${nonce}`)
    expect(await second).toEqual(await first)
    expect(connect).toHaveBeenCalledTimes(1)
  })

  it('names the installation the person chose', async () => {
    const nonce = sentState(lib.beginConnect(AUTHORIZE, origin, 9))
    await returned(`code=c&state=${nonce}`)
    expect(connect).toHaveBeenCalledExactlyOnceWith('org_a', 'c', 9)
  })

  it('asks which installation when the person reaches several', async () => {
    const installations = [{ installation_id: 7, account_login: 'acme' }, { installation_id: 9, account_login: 'lab' }]
    connect.mockRejectedValue(new ApiError(409, 'name one', 'installation_choice_required', { installations }))
    await returned(`code=c&state=${sentState(lib.beginConnect(AUTHORIZE, origin))}`)
    await vi.waitFor(() => expect(state()).toEqual({ state: 'choice', orgID: 'org_a', installations }))
  })

  it('ends with the API\'s words and reason when it refuses', async () => {
    connect.mockRejectedValue(new ApiError(422, 'Ask an admin of the GitHub organisation acme to connect it.', 'installation_not_controlled'))
    await expect(returned(`code=c&state=${sentState(lib.beginConnect(AUTHORIZE, origin))}`)).resolves.toEqual({ to: 'settings', projectID: 'prj_1' })
    expect(state()).toEqual({ state: 'failed', orgID: 'org_a', message: 'Ask an admin of the GitHub organisation acme to connect it.', reason: 'installation_not_controlled' })
  })

  it('goes on to install the App when the person controls no account the org has not connected', async () => {
    connect.mockRejectedValue(new ApiError(422, 'Install the GitHub App', 'app_not_installed'))
    await expect(returned(`code=c&state=${sentState(lib.beginConnect(AUTHORIZE, origin))}`)).resolves.toEqual({ to: 'github', address: INSTALL })
    expect(state()).toEqual({ state: 'idle' })

    // GitHub returns from installing, and the person authorises for the installation it made.
    const next = await returned('installation_id=4242&setup_action=install')
    if (next.to !== 'github') throw new Error('expected to go on to GitHub')
    connect.mockResolvedValue(installation)
    await returned(`code=c2&state=${sentState(next.address)}`)
    expect(connect).toHaveBeenLastCalledWith('org_a', 'c2', 4242)
    expect(state()).toEqual({ state: 'connected', orgID: 'org_a', installation })
  })

  it('does not go round again when the installation it named is not reached', async () => {
    connect.mockRejectedValue(new ApiError(422, 'Install the GitHub App', 'app_not_installed'))
    await expect(returned(`code=c&state=${sentState(lib.beginConnect(AUTHORIZE, origin, 4242))}`)).resolves.toEqual({ to: 'settings', projectID: 'prj_1' })
    expect(state()).toEqual({ state: 'failed', orgID: 'org_a', message: 'Install the App on the GitHub account to connect, then connect again.', reason: 'app_not_installed' })
  })

  it('ends with GitHub\'s words when the person declined, and sends nothing', async () => {
    const nonce = sentState(lib.beginConnect(AUTHORIZE, origin))
    await returned(`error=access_denied&error_description=The+user+has+denied+access&state=${nonce}`)
    expect(state()).toEqual({ state: 'failed', orgID: 'org_a', message: 'GitHub did not authorise the App: The user has denied access' })
    expect(connect).not.toHaveBeenCalled()
  })

  it.each([
    ['another state', 'code=stolen&state=forged'],
    ['no state', 'code=stolen'],
  ])('is refused with %s than the one this tab sent, and sends nothing', async (_, query) => {
    lib.beginConnect(AUTHORIZE, origin)
    await expect(returned(query)).resolves.toEqual({ to: 'settings', projectID: 'prj_1' })
    expect(state().state).toBe('refused')
    expect(connect).not.toHaveBeenCalled()
  })

  it('is refused when this tab started nothing', async () => {
    await expect(returned('code=stolen&state=forged')).resolves.toEqual({ to: 'settings', projectID: null })
    expect(state().state).toBe('refused')
    expect(connect).not.toHaveBeenCalled()
  })

  it('works once: the same return again finds no attempt', async () => {
    const nonce = sentState(lib.beginConnect(AUTHORIZE, origin))
    await returned(`code=code-1&state=${nonce}`)
    await returned(`code=code-1&state=${nonce}&again=1`)
    expect(connect).toHaveBeenCalledTimes(1)
    expect(state().state).toBe('refused')
  })
})

describe('a return from installing the App', () => {
  it('goes on to authorise for the installation GitHub made, when this tab left to install', async () => {
    expect(lib.beginInstall(INSTALL, origin)).toBe(INSTALL)
    const next = await returned('installation_id=4242&setup_action=install')
    if (next.to !== 'github') throw new Error('expected to go on to GitHub')
    expect(next.address.startsWith(AUTHORIZE)).toBe(true)
    expect(connect).not.toHaveBeenCalled()

    await returned(`code=c&state=${sentState(next.address)}`)
    expect(connect).toHaveBeenCalledExactlyOnceWith('org_a', 'c', 4242)
  })

  it('goes on to authorise when GitHub authorised during the installation and returned a code with no state', async () => {
    lib.beginInstall(INSTALL, origin)
    const next = await returned('code=install-code&installation_id=4242&setup_action=install')
    if (next.to !== 'github') throw new Error('expected to go on to GitHub')
    expect(next.address.startsWith(AUTHORIZE)).toBe(true)
    expect(connect).not.toHaveBeenCalled()
  })

  it('says what to do when an organisation owner was asked to install the App', async () => {
    lib.beginInstall(INSTALL, origin)
    await expect(returned('setup_action=request')).resolves.toEqual({ to: 'settings', projectID: 'prj_1' })
    expect(state()).toEqual({ state: 'failed', orgID: 'org_a', message: 'GitHub asked an owner of that organisation to install the App: connect again once they have.' })
    expect(connect).not.toHaveBeenCalled()
  })

  it('does nothing with an installation this tab did not leave to make', async () => {
    await expect(returned('installation_id=4242&setup_action=install')).resolves.toEqual({ to: 'settings', projectID: null })
    expect(state()).toEqual({ state: 'idle' })
    expect(app).not.toHaveBeenCalled()
    expect(connect).not.toHaveBeenCalled()
  })

  it('does not go on for an attempt that left to authorise', async () => {
    lib.beginConnect(AUTHORIZE, origin)
    await expect(returned('installation_id=4242&setup_action=install')).resolves.toEqual({ to: 'settings', projectID: 'prj_1' })
    expect(app).not.toHaveBeenCalled()
  })

  it('says so when the App\'s addresses cannot be read', async () => {
    app.mockRejectedValue(new Error('API unreachable'))
    lib.beginInstall(INSTALL, origin)
    await expect(returned('installation_id=4242&setup_action=install')).resolves.toEqual({ to: 'settings', projectID: 'prj_1' })
    expect(state()).toEqual({ state: 'failed', orgID: 'org_a', message: 'API unreachable' })
  })
})
