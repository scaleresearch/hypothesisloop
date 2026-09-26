import { beforeEach, describe, expect, it } from 'vitest'
import { dropInvite, heldInvite, inviteLink } from '@/lib/invite'

beforeEach(() => {
  window.sessionStorage.clear()
  window.history.replaceState(null, '', '/invite')
})

describe('an invite link', () => {
  it('is this origin\'s invite page with the token in the fragment, which no server is sent', () => {
    const link = new URL(inviteLink('hli_abc'))
    expect(link.origin).toBe(window.location.origin)
    expect(link.pathname).toBe('/invite')
    expect(link.search).toBe('')
    expect(link.hash).toBe('#hli_abc')
  })
})

describe('the invite a tab holds', () => {
  it('is taken from the address once and read from the tab afterwards', () => {
    window.history.replaceState(null, '', '/invite#hli_abc')
    expect(heldInvite()).toBe('hli_abc')
    expect(window.location.href).toBe(`${window.location.origin}/invite`)
    expect(heldInvite()).toBe('hli_abc')
  })

  it('is replaced by the token of a link opened later', () => {
    window.history.replaceState(null, '', '/invite#hli_abc')
    heldInvite()
    window.history.replaceState(null, '', '/invite#hli_def')
    expect(heldInvite()).toBe('hli_def')
  })

  it('is none when no link was opened, and none once dropped', () => {
    expect(heldInvite()).toBeNull()
    window.history.replaceState(null, '', '/invite#hli_abc')
    heldInvite()
    dropInvite()
    expect(heldInvite()).toBeNull()
  })
})
