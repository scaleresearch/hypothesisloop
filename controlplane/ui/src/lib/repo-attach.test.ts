import { beforeEach, describe, expect, it } from 'vitest'
import { attachStep, authorizeAddress, endAttach, installAddress } from '@/lib/repo-attach'

const INSTALL = 'https://github.test/apps/hl/installations/new'
const AUTHORIZE = 'https://github.test/login/oauth/authorize?client_id=Iv1.abc'

function back(query: string) {
  return attachStep(new URLSearchParams(query))
}

// Starts an attempt and returns the state GitHub is handed.
function begin(projectID = 'prj_2') {
  return new URL(installAddress(INSTALL, projectID)).searchParams.get('state')!
}

beforeEach(() => window.sessionStorage.clear())

describe('leaving for GitHub', () => {
  it('installs with a state that is new each time', () => {
    const first = begin()
    const second = begin()
    expect(first).toMatch(/^[0-9a-f-]{36}$/)
    expect(second).not.toBe(first)
  })

  it('authorises with the way back and a state that carries the installation', () => {
    const state = begin()
    const address = new URL(authorizeAddress(AUTHORIZE, 4242))
    expect(address.origin + address.pathname).toBe('https://github.test/login/oauth/authorize')
    expect(address.searchParams.get('client_id')).toBe('Iv1.abc')
    expect(address.searchParams.get('redirect_uri')).toBe(`${window.location.origin}/settings/repository`)
    expect(address.searchParams.get('state')).toBe(`${state}.4242`)
  })

  it('does not authorise without an attempt in progress', () => {
    expect(() => authorizeAddress(AUTHORIZE, 4242)).toThrow('no attach is in progress in this tab')
  })
})

describe('a return from GitHub', () => {
  it('is no return at all on a plain visit', () => {
    expect(back('project=prj_1')).toEqual({ step: 'start' })
  })

  it('after installing leads to authorising, in the project the attempt began in', () => {
    const state = begin('prj_2')
    expect(back(`installation_id=4242&setup_action=install&state=${state}`)).toEqual({ step: 'authorize', projectID: 'prj_2', installationID: 4242 })
  })

  it('after authorising leads to attaching, with the installation the state carried', () => {
    const state = begin('prj_2')
    expect(back(`code=c1&state=${state}.4242`)).toEqual({ step: 'attach', projectID: 'prj_2', installationID: 4242, code: 'c1' })
  })

  it.each([
    ['with no state', 'code=c1'],
    ['with another state', 'code=c1&state=11111111-1111-1111-1111-111111111111.4242'],
    ['with an installation and no state', 'installation_id=4242&setup_action=install'],
    ['with an installation and another state', 'installation_id=4242&state=forged'],
  ])('is refused %s', (_, query) => {
    begin()
    expect(back(query)).toMatchObject({ step: 'refused', reason: expect.stringContaining('does not belong to an attach started in this tab') })
  })

  it('is refused when this tab started nothing, whatever the state says', () => {
    expect(back('code=c1&state=anything.4242').step).toBe('refused')
    expect(back('installation_id=4242&state=').step).toBe('refused')
  })

  it('is refused once the attempt has ended', () => {
    const state = begin()
    endAttach()
    expect(back(`code=c1&state=${state}.4242`).step).toBe('refused')
  })

  it.each(['', 'abc', '0', '-4', '42.5', '4242.1'])('is refused with a code and the installation "%s"', carried => {
    const state = begin()
    expect(back(`code=c1&state=${state}.${carried}`).step).toBe('refused')
    expect(back(`code=c1&state=${state}`).step).toBe('refused')
  })

  it('is refused, saying why, when installing awaits approval and names no installation', () => {
    const state = begin()
    expect(back(`setup_action=request&state=${state}`)).toMatchObject({ step: 'refused', reason: expect.stringContaining('GitHub returned without an installation') })
    expect(back(`installation_id=abc&state=${state}`).step).toBe('refused')
  })

  it('is refused in GitHub\'s words when the person declined to authorise', () => {
    const state = begin()
    expect(back(`error=access_denied&error_description=The+user+has+denied+access&state=${state}.4242`)).toEqual({
      step: 'refused', reason: 'GitHub did not authorise the App: The user has denied access',
    })
  })
})
