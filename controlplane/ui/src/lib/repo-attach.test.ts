import { renderHook } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { beginAttach, isReturn, resetAttach, takeReturn, useAttach, validRepository } from '@/lib/repo-attach'
import type { ProjectRepo } from '@/types'

const AUTHORIZE = 'https://github.test/login/oauth/authorize?client_id=Iv1.abc'
const view = { project_id: 'prj_2', repo: 'acme/widgets' } as ProjectRepo

const attach = vi.fn<(projectID: string, repo: string, code: string) => Promise<ProjectRepo>>()

function now() {
  return renderHook(() => useAttach()).result.current
}

// Starts an attempt and returns the state GitHub is handed.
function begin(projectID = 'prj_2', repo = 'acme/widgets') {
  return new URL(beginAttach(AUTHORIZE, projectID, repo)).searchParams.get('state')!
}

function back(query: string) {
  return takeReturn(new URLSearchParams(query), attach)
}

beforeEach(() => {
  window.sessionStorage.clear()
  resetAttach()
  attach.mockResolvedValue(view)
})

describe('a repository name', () => {
  it.each(['acme/widgets', 'a/b', 'Acme-1/wid.gets_2-x'])('%s is owner/name', name => {
    expect(validRepository(name)).toBe(true)
  })

  it.each(['', 'widgets', 'acme/', '/widgets', 'acme/wid/gets', 'acme/..', 'acme/.', '-acme/widgets', 'acme/wid gets', 'https://github.com/acme/widgets', `acme/${'w'.repeat(200)}`])('"%s" is refused', name => {
    expect(validRepository(name)).toBe(false)
  })
})

describe('leaving for GitHub', () => {
  it('names the way back and a state that is new each time', () => {
    const address = new URL(beginAttach(AUTHORIZE, 'prj_2', 'acme/widgets'))
    expect(address.origin + address.pathname).toBe('https://github.test/login/oauth/authorize')
    expect(address.searchParams.get('client_id')).toBe('Iv1.abc')
    expect(address.searchParams.get('redirect_uri')).toBe(`${window.location.origin}/settings/repository`)
    expect(address.searchParams.get('state')).toMatch(/^[0-9a-f-]{36}$/)
    expect(begin()).not.toBe(address.searchParams.get('state'))
  })

  it('keeps the project and the name in this tab for the return', () => {
    const state = begin('prj_2', 'acme/widgets')
    expect(JSON.parse(window.sessionStorage.getItem('hl-repository-attach')!)).toEqual({ nonce: state, projectID: 'prj_2', repo: 'acme/widgets' })
  })
})

describe('a return from GitHub', () => {
  it('is recognised by a code, a state or an error, and an install return is none', () => {
    expect(isReturn(new URLSearchParams('code=c&state=s'))).toBe(true)
    expect(isReturn(new URLSearchParams('error=access_denied'))).toBe(true)
    expect(isReturn(new URLSearchParams('state=s'))).toBe(true)
    expect(isReturn(new URLSearchParams('installation_id=4242&setup_action=install&project=prj_1'))).toBe(false)
  })

  it('with the attempt\'s state attaches the repository the attempt named, in its project', async () => {
    const state = begin('prj_2', 'acme/widgets')
    expect(back(`code=c1&state=${state}`)).toBe('prj_2')
    expect(attach).toHaveBeenCalledExactlyOnceWith('prj_2', 'acme/widgets', 'c1')
    expect(now()).toEqual({ state: 'attaching', projectID: 'prj_2', repo: 'acme/widgets' })
    await vi.waitFor(() => expect(now()).toEqual({ state: 'attached', projectID: 'prj_2', view }))
  })

  it('is used once: the attempt is gone and a second read of the same return sends nothing', () => {
    const state = begin()
    back(`code=c1&state=${state}`)
    expect(window.sessionStorage.getItem('hl-repository-attach')).toBeNull()
    expect(back(`code=c1&state=${state}`)).toBe('prj_2')
    expect(attach).toHaveBeenCalledTimes(1)
  })

  it('is refused after a reload, when the attempt was already used', () => {
    const state = begin()
    back(`code=c1&state=${state}`)
    resetAttach()
    expect(back(`code=c1&state=${state}`)).toBeNull()
    expect(now().state).toBe('refused')
    expect(attach).toHaveBeenCalledTimes(1)
  })

  it.each([
    ['with no state', 'code=c1'],
    ['with another state', 'code=c1&state=11111111-1111-1111-1111-111111111111'],
    ['with an error and another state', 'error=access_denied&state=forged'],
  ])('is refused %s, sends nothing and leaves the attempt for its own return', (_, query) => {
    const state = begin()
    expect(back(query)).toBeNull()
    expect(now()).toMatchObject({ state: 'refused', message: expect.stringContaining('does not belong to an attach started in this tab') })
    expect(attach).not.toHaveBeenCalled()
    expect(JSON.parse(window.sessionStorage.getItem('hl-repository-attach')!).nonce).toBe(state)
  })

  it('is refused when this tab started nothing', () => {
    expect(back('code=c1&state=anything')).toBeNull()
    expect(now().state).toBe('refused')
    expect(attach).not.toHaveBeenCalled()
  })

  it('ends the attempt in GitHub\'s words when the person declined, keeping the name', () => {
    const state = begin('prj_2', 'acme/widgets')
    expect(back(`error=access_denied&error_description=The+user+has+denied+access&state=${state}`)).toBe('prj_2')
    expect(now()).toEqual({ state: 'failed', projectID: 'prj_2', repo: 'acme/widgets', message: 'GitHub did not authorise the App: The user has denied access' })
    expect(attach).not.toHaveBeenCalled()
    expect(window.sessionStorage.getItem('hl-repository-attach')).toBeNull()
  })

  it('ends in the API\'s words and reason when the attach is refused, keeping the name', async () => {
    attach.mockRejectedValue(Object.assign(new Error('the GitHub App is not installed on acme/widgets'), { reason: 'app_not_installed' }))
    back(`code=c1&state=${begin()}`)
    await vi.waitFor(() => expect(now()).toEqual({
      state: 'failed', projectID: 'prj_2', repo: 'acme/widgets', message: 'the GitHub App is not installed on acme/widgets', reason: 'app_not_installed',
    }))
  })

  it('makes way for a new attempt', () => {
    back('code=c1&state=forged')
    begin()
    expect(now()).toEqual({ state: 'idle' })
  })
})
